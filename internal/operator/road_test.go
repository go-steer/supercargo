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
	"strings"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/backend"
	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
)

// The road leg of the door-to-door story: Meridian books Serra to truck a
// container from Santos to Cajamar. "Now" is the 20th, before the ship
// arrives.
var roadNow = time.Date(2026, 10, 20, 9, 30, 0, 0, time.UTC)

const (
	meridian = "meridian-forwarding.example"
	serra    = "serra-transportes.example"
)

func trucks() *TruckBookings {
	return &TruckBookings{RefPrefix: "SR-BKG", Transit: 30 * time.Hour, Lead: 2 * time.Hour,
		CTe: &CTeIssuer{CNPJ: "12345678000190", State: 35}, Now: func() time.Time { return roadNow }}
}

func roadServices() (*RoadBookingService, *RoadAmendmentService, *store.Bookings) {
	bookings, t := store.NewBookings(), trucks()
	return &RoadBookingService{Bookings: bookings, Backend: t, Deny: map[string]bool{"blocked.example": true}, Now: func() time.Time { return roadNow }},
		&RoadAmendmentService{Bookings: bookings, Backend: t}, bookings
}

func roadBody() *intents.RoadBookingRequestBody {
	return &intents.RoadBookingRequestBody{
		ShipperRef:          "MF-0007-L3",
		Shipper:             intents.Party{Name: "Meridian Forwarding", Domain: meridian},
		Pickup:              intents.Stop{Facility: "Santos Brasil Tecon terminal", UNLocationCode: "BRSSZ", Earliest: "2026-10-24T14:00:00Z"},
		Delivery:            intents.Stop{Facility: "Cymbal Calçados DC, dock 2", UNLocationCode: "BRSAO", Latest: "2026-10-27T12:00:00Z"},
		Equipment:           []intents.Equipment{{ISOType: "45G1", Count: 1, GrossWeightKg: 12000}},
		EquipmentReferences: []string{"OCBU0000004"},
		Cargo:               intents.Cargo{Description: "Footwear (sneakers)", HSCode: "6404"},
		Subscription:        &intents.Subscription{Endpoint: "https://" + meridian + "/ulp", Intents: []string{intents.TnTEvent, intents.TnTAlert}},
	}
}

func roadRequest(sender string, b *intents.RoadBookingRequestBody) *node.Request {
	h := envelope.NewHeader(intents.RoadBookingRequest, sender, serra, roadNow)
	return &node.Request{Envelope: &envelope.Envelope{Header: h}, Body: b}
}

func amendment(sender, ref string, a *intents.RoadBookingAmendmentBody) *node.Request {
	h := envelope.NewHeader(intents.RoadBookingAmendment, sender, serra, roadNow)
	h.BookingRef = &ref
	a.BookingRef = ref
	return &node.Request{Envelope: &envelope.Envelope{Header: h}, Body: a}
}

func forbidden(t *testing.T, name string, err error) {
	t.Helper()
	var r *node.Rejection
	if !errors.As(err, &r) || r.Code != intents.CodeForbidden {
		t.Errorf("%s: %v, want a forbidden Rejection", name, err)
	}
}

// TestRoadBooking: a leg is planned in its windows, stored with its CT-e
// and subscription, and confirmed.
func TestRoadBooking(t *testing.T) {
	s, _, bookings := roadServices()
	var booked []store.Booking
	s.OnBooked = func(b store.Booking) { booked = append(booked, b) }
	req := roadRequest(meridian, roadBody())
	if err := s.Authorize(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	reply, err := s.Handle(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	c := reply.Body.(*intents.RoadBookingConfirmationBody)
	if reply.BookingRef != "SR-BKG-0913" || c.Status != intents.StatusConfirmed || c.PlannedPickup != "2026-10-24T14:00:00Z" ||
		c.PlannedDelivery != "2026-10-25T20:00:00Z" || c.SubscriptionID != "SUB-SR-BKG-0913" || len(c.FiscalDocuments) != 1 {
		t.Fatalf("reply %s %+v", reply.BookingRef, c)
	}
	if key := c.FiscalDocuments[0].AccessKey; !strings.HasPrefix(key, "352610"+"12345678000190"+"57001000000913") {
		t.Errorf("access key %s", key)
	}
	b, err := bookings.Get(serra, "SR-BKG-0913")
	if err != nil || b.Road == nil || b.Shipper != meridian || b.Road.Confirmation.PlannedPickup != c.PlannedPickup || len(booked) != 1 {
		t.Fatalf("stored %+v, %v; booked %d", b, err, len(booked))
	}
	if from, to := b.Places(); from != "BRSSZ" || to != "BRSAO" || b.Subscription() == nil || b.SubscriptionID() != "SUB-SR-BKG-0913" ||
		b.EquipmentRefs()[0] != "OCBU0000004" || b.ShipperRef() != "MF-0007-L3" {
		t.Errorf("booking views: %s→%s, %v", from, to, b.EquipmentRefs())
	}
	// The stored copy is the store's own.
	b.Road.Request.EquipmentReferences[0] = "XXXX0000000"
	if again, _ := bookings.Get(serra, "SR-BKG-0913"); again.EquipmentRefs()[0] != "OCBU0000004" {
		t.Error("a caller changed the stored leg")
	}
}

// TestRoadBookingAuthorize: a denied sender and a subscription to another
// domain are refused before anything is planned.
func TestRoadBookingAuthorize(t *testing.T) {
	s, _, _ := roadServices()
	forbidden(t, "denied sender", s.Authorize(context.Background(), roadRequest("blocked.example", roadBody())))
	b := roadBody()
	b.Subscription.Endpoint = "https://victim.example/ulp"
	forbidden(t, "subscription elsewhere", s.Authorize(context.Background(), roadRequest(meridian, b)))
}

// TestTruckPlans: the pickup is as early as notice and the window allow;
// a leg that can't make its windows is declined, and nothing is stored.
func TestTruckPlans(t *testing.T) {
	for name, tc := range map[string]struct {
		edit             func(*intents.RoadBookingRequestBody)
		pickup, delivery string
		code             string
	}{
		"no windows: the lead, on the hour": {func(b *intents.RoadBookingRequestBody) { b.Pickup.Earliest, b.Delivery.Latest = "", "" }, "2026-10-20T12:00:00Z", "2026-10-21T18:00:00Z", ""},
		"a delivery window to wait for":     {func(b *intents.RoadBookingRequestBody) { b.Delivery.Earliest = "2026-10-26T08:00:00Z" }, "2026-10-24T14:00:00Z", "2026-10-26T08:00:00Z", ""},
		"a pickup window already gone": {func(b *intents.RoadBookingRequestBody) {
			b.Pickup.Earliest, b.Pickup.Latest = "", "2026-10-20T10:00:00Z"
		}, "", "", "pickup_window_missed"},
		"a delivery too soon": {func(b *intents.RoadBookingRequestBody) { b.Delivery.Latest = "2026-10-25T08:00:00Z" }, "", "", "delivery_window_missed"},
	} {
		s, _, bookings := roadServices()
		b := roadBody()
		tc.edit(b)
		reply, err := s.Handle(context.Background(), roadRequest(meridian, b))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		c := reply.Body.(*intents.RoadBookingConfirmationBody)
		if tc.code != "" {
			if c.Status != intents.StatusRejected || len(c.Messages) != 1 || c.Messages[0].Code != tc.code || bookings.Len() != 0 {
				t.Errorf("%s: %+v, %d stored", name, c, bookings.Len())
			}
			continue
		}
		if c.PlannedPickup != tc.pickup || c.PlannedDelivery != tc.delivery {
			t.Errorf("%s: %s → %s", name, c.PlannedPickup, c.PlannedDelivery)
		}
	}
}

// TestTruckDocuments: an e-CMR for a European carrier; none without
// configuration; no subscription ID without a subscription.
func TestTruckDocuments(t *testing.T) {
	tb := trucks()
	tb.CTe, tb.ECMRPrefix = nil, "ECMR-NL-2026"
	b := roadBody()
	b.Subscription = nil
	c, err := tb.Decide(context.Background(), backend.RoadBookingRequest{Shipper: meridian, Operator: serra, Body: b})
	if err != nil || len(c.FiscalDocuments) != 0 || len(c.TransportDocuments) != 1 || c.TransportDocuments[0].Number != "ECMR-NL-2026-000913" || c.SubscriptionID != "" {
		t.Fatalf("%+v, %v", c, err)
	}
	if err := c.Validate(); err != nil {
		t.Error(err)
	}
	tb.CTe = &CTeIssuer{CNPJ: "123", State: 35}
	if _, err := tb.Decide(context.Background(), backend.RoadBookingRequest{Body: b}); err == nil {
		t.Error("a malformed CT-e issuer")
	}
}

// TestCTeAccessKey: the check digit is the published one, letters in an
// alphanumeric CNPJ count, and the key is one the road bodies accept.
func TestCTeAccessKey(t *testing.T) {
	// The NF-e/CT-e manual's example key, whose check digit is 5.
	if d := mod11("5206043300991100250655012000000780026730161"); d != 5 {
		t.Errorf("check digit %d, want 5", d)
	}
	if mod11("0000000000000000000000000000000000000000000") != 0 || mod11("00000000000000000000000000000000000000000A1") == mod11("0000000000000000000000000000000000000000001") {
		t.Error("mod11 doesn't weigh letters")
	}
	for _, cnpj := range []string{"12345678000190", "12ABC34501DE35"} {
		key, err := (&CTeIssuer{CNPJ: cnpj, State: 35}).accessKey(roadNow, 913, "57")
		if err != nil || len(key) != 44 || int(key[43]-'0') != mod11(key[:43]) {
			t.Fatalf("%s: %s, %v", cnpj, key, err)
		}
		c := intents.RoadBookingConfirmationBody{BookingRef: "SR-BKG-0913", Status: intents.StatusConfirmed,
			PlannedPickup: "2026-10-24T14:00:00Z", PlannedDelivery: "2026-10-25T20:00:00Z",
			FiscalDocuments: []intents.DocumentRef{{Type: "CT-e", AccessKey: key}}}
		if err := c.Validate(); err != nil {
			t.Errorf("%s: %v", cnpj, err)
		}
	}
	if _, err := (&CTeIssuer{CNPJ: "12345678000190", State: 99}).accessKey(roadNow, 1, "57"); err == nil {
		t.Error("state 99")
	}
}

// TestRoadAmendment: the leg's shipper moves its windows; the leg is
// planned again under its reference, keeping its documents.
func TestRoadAmendment(t *testing.T) {
	s, am, bookings := roadServices()
	var amended []store.Booking
	am.OnAmended = func(b store.Booking) { amended = append(amended, b) }
	first, err := s.Handle(context.Background(), roadRequest(meridian, roadBody()))
	if err != nil {
		t.Fatal(err)
	}
	was := first.Body.(*intents.RoadBookingConfirmationBody)
	// The ship is 72 hours late: collect from the 27th, deliver by the 29th.
	req := amendment(meridian, "SR-BKG-0913", &intents.RoadBookingAmendmentBody{
		Pickup: &intents.Window{Earliest: "2026-10-27T14:00:00Z"}, Delivery: &intents.Window{Latest: "2026-10-29T12:00:00Z"}})
	if err := am.Authorize(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	reply, err := am.Handle(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	c := reply.Body.(*intents.RoadBookingConfirmationBody)
	if reply.BookingRef != "SR-BKG-0913" || c.Status != intents.StatusConfirmed || c.PlannedPickup != "2026-10-27T14:00:00Z" ||
		c.PlannedDelivery != "2026-10-28T20:00:00Z" || c.FiscalDocuments[0] != was.FiscalDocuments[0] || c.SubscriptionID != "SUB-SR-BKG-0913" {
		t.Fatalf("reply %+v", c)
	}
	b, _ := bookings.Get(serra, "SR-BKG-0913")
	if b.Road.Request.Pickup.Earliest != "2026-10-27T14:00:00Z" || b.Road.Request.Delivery.Latest != "2026-10-29T12:00:00Z" ||
		b.Road.Confirmation.PlannedDelivery != c.PlannedDelivery || len(amended) != 1 {
		t.Errorf("stored %+v", b.Road)
	}
}

// TestRoadAmendmentRefused: only the leg's shipper may amend it, and an
// unknown leg, an ocean booking and someone else's leg look the same; a
// plan that can't be run, or windows that don't fit, leave it unchanged.
func TestRoadAmendmentRefused(t *testing.T) {
	s, am, bookings := roadServices()
	if _, err := s.Handle(context.Background(), roadRequest(meridian, roadBody())); err != nil {
		t.Fatal(err)
	}
	_ = bookings.Add(store.Booking{Ref: "SR-BKG-0001", Shipper: meridian, Operator: serra}) // an ocean booking
	move := func() *intents.RoadBookingAmendmentBody {
		return &intents.RoadBookingAmendmentBody{Pickup: &intents.Window{Earliest: "2026-10-27T14:00:00Z"}}
	}
	var details []string
	for name, req := range map[string]*node.Request{
		"another sender":   amendment("mallory.example", "SR-BKG-0913", move()),
		"an unknown leg":   amendment(meridian, "SR-BKG-9999", move()),
		"an ocean booking": amendment(meridian, "SR-BKG-0001", move()),
		"another's operator": func() *node.Request {
			r := amendment(meridian, "SR-BKG-0913", move())
			r.Envelope.Header.Receiver = "other.example"
			return r
		}(),
	} {
		err := am.Authorize(context.Background(), req)
		forbidden(t, name, err)
		var r *node.Rejection
		if errors.As(err, &r) {
			details = append(details, r.Detail)
		}
	}
	for _, d := range details {
		if d != details[0] {
			t.Errorf("refusals differ: %q vs %q", d, details[0])
		}
	}
	for name, tc := range map[string]struct {
		a    *intents.RoadBookingAmendmentBody
		code string
	}{
		"a delivery too soon":    {&intents.RoadBookingAmendmentBody{Delivery: &intents.Window{Latest: "2026-10-25T08:00:00Z"}}, "delivery_window_missed"},
		"windows that don't fit": {&intents.RoadBookingAmendmentBody{Delivery: &intents.Window{Latest: "2026-10-23T08:00:00Z"}}, "invalid_windows"},
	} {
		reply, err := am.Handle(context.Background(), amendment(meridian, "SR-BKG-0913", tc.a))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		c := reply.Body.(*intents.RoadBookingConfirmationBody)
		if c.Status != intents.StatusRejected || c.Messages[0].Code != tc.code || c.BookingRef != "SR-BKG-0913" {
			t.Errorf("%s: %+v", name, c)
		}
		if err := c.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if b, _ := bookings.Get(serra, "SR-BKG-0913"); b.Road.Confirmation.PlannedDelivery != "2026-10-25T20:00:00Z" || b.Road.Request.Delivery.Latest != "2026-10-27T12:00:00Z" {
			t.Errorf("%s changed the leg: %+v", name, b.Road)
		}
	}
}

// badRoad is a booking system with a bug.
type badRoad struct {
	conf *intents.RoadBookingConfirmationBody
}

func (b badRoad) Decide(context.Context, backend.RoadBookingRequest) (*intents.RoadBookingConfirmationBody, error) {
	return b.conf, nil
}

func (b badRoad) Amend(context.Context, backend.RoadAmendment) (*intents.RoadBookingConfirmationBody, error) {
	return b.conf, nil
}

// TestRoadBackendChecked: a booking system's malformed, empty or
// mismatched answer is never stored or signed.
func TestRoadBackendChecked(t *testing.T) {
	ok := &intents.RoadBookingConfirmationBody{BookingRef: "SR-BKG-0913", Status: intents.StatusConfirmed,
		PlannedPickup: "2026-10-24T14:00:00Z", PlannedDelivery: "2026-10-25T20:00:00Z", SubscriptionID: "SUB-1"}
	noSub := *ok
	noSub.SubscriptionID = ""
	bad := *ok
	bad.PlannedDelivery = "2026-10-23T00:00:00Z"
	other := *ok
	other.BookingRef = "SR-BKG-0001"
	for name, conf := range map[string]*intents.RoadBookingConfirmationBody{"nil": nil, "invalid": &bad, "no subscription": &noSub} {
		bookings := store.NewBookings()
		s := &RoadBookingService{Bookings: bookings, Backend: badRoad{conf}, Now: time.Now}
		if _, err := s.Handle(context.Background(), roadRequest(meridian, roadBody())); err == nil || bookings.Len() != 0 {
			t.Errorf("%s: %v, %d stored", name, err, bookings.Len())
		}
	}
	// A reference the backend reused is never overwritten.
	bookings := store.NewBookings()
	s := &RoadBookingService{Bookings: bookings, Backend: badRoad{ok}, Now: time.Now}
	if _, err := s.Handle(context.Background(), roadRequest(meridian, roadBody())); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Handle(context.Background(), roadRequest("cymbal-shoes.example", func() *intents.RoadBookingRequestBody {
		b := roadBody()
		b.Shipper.Domain, b.Subscription.Endpoint = "cymbal-shoes.example", "https://cymbal-shoes.example/ulp"
		return b
	}())); !errors.Is(err, store.ErrExists) {
		t.Errorf("a reused reference: %v", err)
	}
	if b, _ := bookings.Get(serra, "SR-BKG-0913"); b.Shipper != meridian {
		t.Error("a reused reference replaced the leg")
	}
	// An amendment answered for another booking, or malformed.
	for name, conf := range map[string]*intents.RoadBookingConfirmationBody{"another booking": &other, "invalid": &bad} {
		am := &RoadAmendmentService{Bookings: bookings, Backend: badRoad{conf}}
		if _, err := am.Handle(context.Background(), amendment(meridian, "SR-BKG-0913", &intents.RoadBookingAmendmentBody{
			Pickup: &intents.Window{Earliest: "2026-10-24T15:00:00Z"}})); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// At capacity: a rejection, not an error.
	full := &RoadBookingService{Bookings: store.NewBookingsCap(0), Backend: trucks(), Now: time.Now}
	reply, err := full.Handle(context.Background(), roadRequest(meridian, roadBody()))
	if err != nil || reply.Body.(*intents.RoadBookingConfirmationBody).Messages[0].Code != "at_capacity" {
		t.Errorf("at capacity: %+v, %v", reply, err)
	}
}

// racingRoad is a booking system during whose planning another amendment
// of the same leg is stored.
type racingRoad struct {
	*TruckBookings
	race func()
}

func (r racingRoad) Amend(ctx context.Context, a backend.RoadAmendment) (*intents.RoadBookingConfirmationBody, error) {
	r.race()
	return r.TruckBookings.Amend(ctx, a)
}

// TestRoadAmendmentRace: an amendment planned while another was stored is
// refused, and the one stored stands: no update is lost.
func TestRoadAmendmentRace(t *testing.T) {
	s, am, bookings := roadServices()
	if _, err := s.Handle(context.Background(), roadRequest(meridian, roadBody())); err != nil {
		t.Fatal(err)
	}
	other := &RoadAmendmentService{Bookings: bookings, Backend: trucks()}
	am.Backend = racingRoad{trucks(), func() {
		// The delivery moves meanwhile.
		if _, err := other.Handle(context.Background(), amendment(meridian, "SR-BKG-0913", &intents.RoadBookingAmendmentBody{
			Delivery: &intents.Window{Latest: "2026-10-28T12:00:00Z"}})); err != nil {
			t.Error(err)
		}
	}}
	reply, err := am.Handle(context.Background(), amendment(meridian, "SR-BKG-0913", &intents.RoadBookingAmendmentBody{
		Pickup: &intents.Window{Earliest: "2026-10-25T14:00:00Z"}}))
	if err != nil {
		t.Fatal(err)
	}
	if c := reply.Body.(*intents.RoadBookingConfirmationBody); c.Status != intents.StatusRejected || c.Messages[0].Code != "changed_meanwhile" {
		t.Errorf("reply %+v", c)
	}
	b, _ := bookings.Get(serra, "SR-BKG-0913")
	if b.Road.Request.Delivery.Latest != "2026-10-28T12:00:00Z" || b.Road.Request.Pickup.Earliest != "2026-10-24T14:00:00Z" {
		t.Errorf("stored %+v", b.Road.Request)
	}
}

// TestTruckPlanEdges: notice ending on the hour isn't rounded further; an
// amendment of the delivery alone keeps the planned pickup.
func TestTruckPlanEdges(t *testing.T) {
	tb := trucks()
	tb.Now = func() time.Time { return time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC) }
	b := roadBody()
	b.Pickup.Earliest, b.Pickup.Latest, b.Delivery.Latest = "", "2026-10-20T12:00:00Z", ""
	c, err := tb.Decide(context.Background(), backend.RoadBookingRequest{Body: b})
	if err != nil || c.Status != intents.StatusConfirmed || c.PlannedPickup != "2026-10-20T12:00:00Z" {
		t.Fatalf("on the hour: %+v, %v", c, err)
	}
	// Two days later, only the delivery moves: the truck still collects
	// when planned, though that's now in the past.
	tb.Now = func() time.Time { return time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC) }
	moved := *b
	moved.Delivery.Earliest = "2026-10-22T20:00:00Z"
	a, err := tb.Amend(context.Background(), backend.RoadAmendment{Request: &moved, Current: c,
		Amendment: &intents.RoadBookingAmendmentBody{Delivery: &intents.Window{Earliest: "2026-10-22T20:00:00Z"}}})
	if err != nil || a.PlannedPickup != "2026-10-20T12:00:00Z" || a.PlannedDelivery != "2026-10-22T20:00:00Z" {
		t.Errorf("delivery only: %+v, %v", a, err)
	}
}

// TestRoadBackendPlanChecked: a backend's plan outside the windows asked
// for, or an amendment that changes the subscription, is never stored.
func TestRoadBackendPlanChecked(t *testing.T) {
	early := &intents.RoadBookingConfirmationBody{BookingRef: "SR-BKG-0913", Status: intents.StatusConfirmed,
		PlannedPickup: "2026-10-23T14:00:00Z", PlannedDelivery: "2026-10-25T20:00:00Z", SubscriptionID: "SUB-1"}
	late := *early
	late.PlannedPickup, late.PlannedDelivery = "2026-10-24T14:00:00Z", "2026-10-28T20:00:00Z"
	for name, conf := range map[string]*intents.RoadBookingConfirmationBody{"before the pickup window": early, "after the delivery window": &late} {
		bookings := store.NewBookings()
		s := &RoadBookingService{Bookings: bookings, Backend: badRoad{conf}, Now: time.Now}
		if _, err := s.Handle(context.Background(), roadRequest(meridian, roadBody())); err == nil || bookings.Len() != 0 {
			t.Errorf("%s: %v", name, err)
		}
	}
	s, _, bookings := roadServices()
	if _, err := s.Handle(context.Background(), roadRequest(meridian, roadBody())); err != nil {
		t.Fatal(err)
	}
	resub := &intents.RoadBookingConfirmationBody{BookingRef: "SR-BKG-0913", Status: intents.StatusConfirmed,
		PlannedPickup: "2026-10-24T15:00:00Z", PlannedDelivery: "2026-10-25T20:00:00Z", SubscriptionID: "SUB-OTHER"}
	am := &RoadAmendmentService{Bookings: bookings, Backend: badRoad{resub}}
	if _, err := am.Handle(context.Background(), amendment(meridian, "SR-BKG-0913", &intents.RoadBookingAmendmentBody{
		Pickup: &intents.Window{Earliest: "2026-10-24T15:00:00Z"}})); err == nil {
		t.Error("a changed subscription was accepted")
	}
}
