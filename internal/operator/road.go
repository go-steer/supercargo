// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package operator

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-steer/supercargo/internal/backend"
	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
)

// RoadBookingService handles ulp.road.booking.request: a road carrier's
// side of a truck leg (docs/DESIGN.md, A.5 "Road bookings"). Like
// BookingService, the protocol's part is here and the decision is the
// backend's.
type RoadBookingService struct {
	Bookings *store.Bookings
	// Deny lists domains whose requests are refused (A.6: as ocean
	// booking requests).
	Deny    map[string]bool
	Backend backend.RoadBookings
	Now     func() time.Time
	// OnBooked runs for each leg stored, before its confirmation is sent.
	// Optional; it must not block.
	OnBooked func(store.Booking)
}

// Authorize implements node.Handler: the sender is not denied, and a
// subscription points at the sender's own domain (A.6).
func (s *RoadBookingService) Authorize(_ context.Context, req *node.Request) error {
	sender := req.Envelope.Header.Sender
	if s.Deny[sender] {
		return node.Reject(intents.CodeForbidden, "This carrier does not accept bookings from the sender.", "sender is on the deny list")
	}
	b := req.Body.(*intents.RoadBookingRequestBody)
	if b.Subscription != nil && b.Subscription.EndpointHost() != sender {
		return node.Reject(intents.CodeForbidden, "The subscription endpoint must be on the sender's own domain.",
			"subscription endpoint host "+b.Subscription.EndpointHost()+" is not "+sender)
	}
	return nil
}

// Handle implements node.Handler: the backend plans the leg; a confirmed
// leg is stored, and either answer goes back to the shipper.
func (s *RoadBookingService) Handle(ctx context.Context, req *node.Request) (*node.Reply, error) {
	h := req.Envelope.Header
	b := req.Body.(*intents.RoadBookingRequestBody)
	conf, err := s.Backend.Decide(ctx, backend.RoadBookingRequest{Shipper: h.Sender, Operator: h.Receiver, Body: b})
	if err != nil {
		return nil, err
	}
	if err := checkRoadAnswer(conf, b); err != nil {
		return nil, err
	}
	if conf.Status != intents.StatusConfirmed {
		return &node.Reply{BookingRef: conf.BookingRef, Body: conf}, nil
	}
	if err := s.Bookings.Add(store.Booking{
		Ref: conf.BookingRef, Shipper: h.Sender, Operator: h.Receiver,
		Road:      &store.RoadBooking{Request: *b, Confirmation: *conf},
		CreatedAt: s.Now(),
	}); errors.Is(err, store.ErrFull) {
		return &node.Reply{BookingRef: conf.BookingRef, Body: &intents.RoadBookingConfirmationBody{
			BookingRef: conf.BookingRef, Status: intents.StatusRejected,
			Messages: []intents.Message{{Code: "at_capacity", Detail: "This carrier is not taking new bookings right now."}},
		}}, nil
	} else if err != nil {
		// ErrExists: the backend reused a reference. Never overwrite
		// another shipper's leg.
		return nil, fmt.Errorf("operator: storing road booking %s: %w", conf.BookingRef, err)
	}
	if s.OnBooked != nil {
		if stored, err := s.Bookings.Get(h.Receiver, conf.BookingRef); err == nil {
			s.OnBooked(stored)
		}
	}
	return &node.Reply{BookingRef: conf.BookingRef, Body: conf}, nil
}

// checkRoadAnswer checks what a backend answered before anything is
// stored or signed: a bug there must not become a live booking or a
// malformed message to a partner.
func checkRoadAnswer(conf *intents.RoadBookingConfirmationBody, b *intents.RoadBookingRequestBody) error {
	if conf == nil {
		return errors.New("operator: the booking system gave no answer")
	}
	if err := conf.Validate(); err != nil {
		return fmt.Errorf("operator: the booking system's answer: %w", err)
	}
	if conf.Status != intents.StatusConfirmed {
		return nil
	}
	if b.Subscription != nil && conf.SubscriptionID == "" {
		return errors.New("operator: the booking system confirmed a subscribed leg without a subscription_id")
	}
	// The plan keeps the windows the shipper asked for: confirming a
	// pickup outside them would promise what wasn't asked.
	if !inWindow(conf.PlannedPickup, b.Pickup) || !inWindow(conf.PlannedDelivery, b.Delivery) {
		return fmt.Errorf("operator: the booking system planned %s → %s outside the leg's windows", conf.PlannedPickup, conf.PlannedDelivery)
	}
	return nil
}

// inWindow reports whether the time at falls in the stop's window.
func inWindow(at string, s intents.Stop) bool {
	t, _ := parseTime(at) // validated
	if e, ok := parseTime(s.Earliest); ok && t.Before(e) {
		return false
	}
	if l, ok := parseTime(s.Latest); ok && t.After(l) {
		return false
	}
	return true
}

// RoadAmendmentService handles ulp.road.booking.amendment: the shipper of
// a leg moves its pickup or delivery window, and the carrier answers with
// a confirmation (the new plan) or a rejection (the old plan stands).
type RoadAmendmentService struct {
	Bookings *store.Bookings
	Backend  backend.RoadBookings
	// OnAmended runs for each leg whose plan changed. Optional; it must
	// not block.
	OnAmended func(store.Booking)
}

// errLegChanged reports a leg amended by someone else while an amendment
// was being planned.
var errLegChanged = errors.New("operator: the leg changed meanwhile")

// forbiddenLeg is the one answer to an amendment of a leg the sender
// doesn't hold, whether it exists or not, so the answer reveals nothing.
var forbiddenLeg = node.Reject(intents.CodeForbidden, "No such booking for the sender.", "")

// Authorize implements node.Handler: the sender is the leg's shipper (A.6).
// The booking is looked up under this carrier, and an unknown leg, an
// ocean booking and another shipper's leg get the same answer.
func (s *RoadAmendmentService) Authorize(_ context.Context, req *node.Request) error {
	h := req.Envelope.Header
	b, err := s.Bookings.Get(h.Receiver, *h.BookingRef)
	switch {
	case err != nil:
		return &node.Rejection{Code: forbiddenLeg.Code, Detail: forbiddenLeg.Detail, Reason: "no booking " + *h.BookingRef}
	case b.Road == nil:
		return &node.Rejection{Code: forbiddenLeg.Code, Detail: forbiddenLeg.Detail, Reason: *h.BookingRef + " is not a road booking"}
	case b.Shipper != h.Sender:
		return &node.Rejection{Code: forbiddenLeg.Code, Detail: forbiddenLeg.Detail, Reason: *h.BookingRef + " is " + b.Shipper + "'s, not " + h.Sender + "'s"}
	}
	return nil
}

// Handle implements node.Handler: the new windows go into a copy of the
// leg, the backend decides, and a confirmed plan replaces the stored one.
func (s *RoadAmendmentService) Handle(ctx context.Context, req *node.Request) (*node.Reply, error) {
	h := req.Envelope.Header
	a := req.Body.(*intents.RoadBookingAmendmentBody)
	b, err := s.Bookings.Get(h.Receiver, a.BookingRef)
	if err != nil {
		return nil, err // authorized a moment ago; gone only on a reset
	}
	if b.Cancelled() {
		return &node.Reply{BookingRef: a.BookingRef, Body: &intents.RoadBookingConfirmationBody{
			BookingRef: a.BookingRef, Status: intents.StatusRejected,
			Messages: []intents.Message{{Code: "cancelled", Detail: "The leg was cancelled."}},
		}}, nil
	}
	amended := b.Road.Request
	amendWindow(&amended.Pickup, a.Pickup)
	amendWindow(&amended.Delivery, a.Delivery)
	if err := amended.Validate(); err != nil {
		// The new windows don't fit the leg (a pickup window now ending
		// before it starts, say): the old plan stands.
		return &node.Reply{BookingRef: a.BookingRef, Body: &intents.RoadBookingConfirmationBody{
			BookingRef: a.BookingRef, Status: intents.StatusRejected,
			Messages: []intents.Message{{Code: "invalid_windows", Detail: "The amended windows don't fit the booked leg; the current plan stands."}},
		}}, nil
	}
	conf, err := s.Backend.Amend(ctx, backend.RoadAmendment{Shipper: h.Sender, Operator: h.Receiver,
		Request: &amended, Current: &b.Road.Confirmation, Amendment: a})
	if err != nil {
		return nil, err
	}
	if err := checkRoadAnswer(conf, &amended); err != nil {
		return nil, err
	}
	if conf.BookingRef != a.BookingRef {
		return nil, fmt.Errorf("operator: the booking system answered an amendment of %s for %s", a.BookingRef, conf.BookingRef)
	}
	if conf.Status != intents.StatusConfirmed {
		return &node.Reply{BookingRef: a.BookingRef, Body: conf}, nil
	}
	if conf.SubscriptionID != b.Road.Confirmation.SubscriptionID {
		return nil, fmt.Errorf("operator: the booking system changed %s's subscription in an amendment", a.BookingRef)
	}
	snapshot := b.Road
	stored, err := s.Bookings.Update(h.Receiver, a.BookingRef, func(b *store.Booking) error {
		// Only over the leg this plan was made from: another amendment
		// stored meanwhile would otherwise be lost, and its shipper told a
		// plan that no longer holds.
		if b.Road == nil || b.Shipper != h.Sender || b.Cancelled() || !reflect.DeepEqual(b.Road, snapshot) {
			return errLegChanged
		}
		b.Road.Request, b.Road.Confirmation = amended, *conf
		return nil
	})
	if errors.Is(err, errLegChanged) {
		return &node.Reply{BookingRef: a.BookingRef, Body: &intents.RoadBookingConfirmationBody{
			BookingRef: a.BookingRef, Status: intents.StatusRejected,
			Messages: []intents.Message{{Code: "changed_meanwhile", Detail: "The leg changed while this amendment was planned; the plan confirmed meanwhile stands. Send the amendment again if it still applies."}},
		}}, nil
	} else if err != nil {
		return nil, err
	}
	if s.OnAmended != nil {
		s.OnAmended(stored)
	}
	return &node.Reply{BookingRef: a.BookingRef, Body: conf}, nil
}

// amendWindow puts an amendment's window into a stop: each time it gives
// replaces the stop's, and a time it leaves out stays as it was.
func amendWindow(s *intents.Stop, w *intents.Window) {
	if w == nil {
		return
	}
	if w.Earliest != "" {
		s.Earliest = w.Earliest
	}
	if w.Latest != "" {
		s.Latest = w.Latest
	}
}

// TruckBookings is the in-memory road booking system: it plans each leg
// from its windows (the pickup at the window's start, or at least Lead
// from now on the next full hour if that's later; the delivery Transit
// later, or at the delivery window's start if that's later), and declines
// a leg it can't run inside them. An amendment that moves only the
// delivery keeps the planned pickup. It
// issues a mock CT-e (CTe) and an e-CMR number (ECMRPrefix) when
// configured. It is what a road carrier's TMS would decide; the demo's
// default.
type TruckBookings struct {
	// RefPrefix starts each booking reference, e.g. "SR-BKG".
	RefPrefix string
	Transit   time.Duration
	Lead      time.Duration
	// CTe, if set, issues a CT-e for each confirmed leg (Brazil).
	CTe *CTeIssuer
	// ECMRPrefix, if set, numbers an e-CMR for each confirmed leg
	// (Europe), e.g. "ECMR-NL-2026".
	ECMRPrefix string
	Now        func() time.Time

	mu   sync.Mutex
	next int
}

// CTeIssuer is the issuer of the mock CT-e: its CNPJ and its state's IBGE
// code (35 for São Paulo). The access key has the real layout and check
// digit; nothing is sent to the tax authority.
type CTeIssuer struct {
	CNPJ  string // 14 characters, bare
	State int
}

// Decide implements backend.RoadBookings.
func (t *TruckBookings) Decide(_ context.Context, r backend.RoadBookingRequest) (*intents.RoadBookingConfirmationBody, error) {
	t.mu.Lock()
	if t.next == 0 {
		t.next = 913
	}
	n := t.next
	t.next++
	t.mu.Unlock()
	ref := fmt.Sprintf("%s-%04d", t.RefPrefix, n)
	conf, msg := t.plan(ref, r.Body, time.Time{})
	if msg != nil {
		return &intents.RoadBookingConfirmationBody{BookingRef: ref, Status: intents.StatusRejected, Messages: []intents.Message{*msg}}, nil
	}
	if t.CTe != nil {
		key, err := t.CTe.accessKey(t.Now(), n)
		if err != nil {
			return nil, err
		}
		conf.FiscalDocuments = []intents.DocumentRef{{Type: "CT-e", AccessKey: key}}
	}
	if t.ECMRPrefix != "" {
		conf.TransportDocuments = []intents.DocumentRef{{Type: "e-CMR", Number: fmt.Sprintf("%s-%06d", t.ECMRPrefix, n)}}
	}
	if r.Body.Subscription != nil {
		// From the reference, which is unique in the store: an ocean
		// booking's counter can't produce the same ID.
		conf.SubscriptionID = "SUB-" + ref
	}
	return conf, nil
}

// Amend implements backend.RoadBookings: the leg is planned again in its
// new windows, keeping its reference, documents and subscription.
func (t *TruckBookings) Amend(_ context.Context, r backend.RoadAmendment) (*intents.RoadBookingConfirmationBody, error) {
	var keep time.Time
	if r.Amendment.Pickup == nil {
		// Only the delivery moves: the truck still collects when planned
		// (it may have already).
		keep, _ = parseTime(r.Current.PlannedPickup)
	}
	conf, msg := t.plan(r.Current.BookingRef, r.Request, keep)
	if msg != nil {
		return &intents.RoadBookingConfirmationBody{BookingRef: r.Current.BookingRef, Status: intents.StatusRejected,
			Messages: []intents.Message{{Code: msg.Code, Detail: msg.Detail + " The current plan stands."}}}, nil
	}
	conf.FiscalDocuments, conf.TransportDocuments = r.Current.FiscalDocuments, r.Current.TransportDocuments
	conf.SubscriptionID = r.Current.SubscriptionID
	return conf, nil
}

// plan places the leg in its windows, or says why it can't. A non-zero
// keep is a pickup already planned, which stays.
func (t *TruckBookings) plan(ref string, b *intents.RoadBookingRequestBody, keep time.Time) (*intents.RoadBookingConfirmationBody, *intents.Message) {
	pickup := keep
	if pickup.IsZero() {
		pickup = t.Now().Add(t.Lead).UTC()
		if p := pickup.Truncate(time.Hour); !p.Equal(pickup) {
			pickup = p.Add(time.Hour) // the next full hour
		}
		if e, ok := parseTime(b.Pickup.Earliest); ok && e.After(pickup) {
			pickup = e
		}
		if l, ok := parseTime(b.Pickup.Latest); ok && pickup.After(l) {
			return nil, &intents.Message{Code: "pickup_window_missed", Detail: "This carrier can't collect inside the pickup window."}
		}
	}
	delivery := pickup.Add(t.Transit)
	if e, ok := parseTime(b.Delivery.Earliest); ok && e.After(delivery) {
		delivery = e
	}
	if l, ok := parseTime(b.Delivery.Latest); ok && delivery.After(l) {
		return nil, &intents.Message{Code: "delivery_window_missed", Detail: "This carrier can't deliver inside the delivery window."}
	}
	return &intents.RoadBookingConfirmationBody{
		BookingRef: ref, Status: intents.StatusConfirmed,
		PlannedPickup:   pickup.Format(envelope.TimeLayout),
		PlannedDelivery: delivery.Format(envelope.TimeLayout),
	}, nil
}

func parseTime(s string) (time.Time, bool) {
	t, err := time.Parse(envelope.TimeLayout, s)
	return t, err == nil
}

// accessKey is a CT-e access key in the real layout: the state (2), the
// year and month (4), the issuer's CNPJ (14), the model 57 (2), the series
// (3), the number (9), the emission type (1), a numeric code (8) and a
// mod-11 check digit (1).
func (c *CTeIssuer) accessKey(at time.Time, n int) (string, error) {
	if c.State < 11 || c.State > 53 || len(c.CNPJ) != 14 {
		return "", fmt.Errorf("operator: CT-e issuer state %d / CNPJ %q", c.State, c.CNPJ)
	}
	body := fmt.Sprintf("%02d%s%s57001%09d1%08d", c.State, at.In(brasilia).Format("0601"), strings.ToUpper(c.CNPJ), n%1_000_000_000, (n*7919+1)%100_000_000)
	return body + strconv.Itoa(mod11(body)), nil
}

// brasilia is Brasília time, the year and month of a CT-e issued in São
// Paulo (UTC-3; Brazil has kept no daylight saving time since 2019).
var brasilia = time.FixedZone("BRT", -3*60*60)

// mod11 is the CT-e access key's check digit: the characters' values
// (ASCII minus 48, which keeps digits and gives the alphanumeric CNPJ's
// letters their value) weighted 2 to 9 from the right, cycling; 11 minus
// the remainder, or 0 when that's 10 or 11.
func mod11(s string) int {
	sum, w := 0, 2
	for i := len(s) - 1; i >= 0; i-- {
		sum += int(s[i]-'0') * w
		if w++; w > 9 {
			w = 2
		}
	}
	if d := 11 - sum%11; d < 10 {
		return d
	}
	return 0
}
