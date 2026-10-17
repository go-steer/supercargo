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

package shipper

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/delegation"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
)

// Delegated access (docs/DESIGN.md, A.5 "Delegated access"; milestone 7),
// the customer side's two parts. As a booking's shipper, Grant hands
// another company a delegation for it: the step a person confirms. As a
// grantee, the Grants handler takes a grant handed to this node, keeps the
// booking as a delegated one, and subscribes to its events with the
// carrier; Ask and Status carry the token for such a booking.

var (
	// ErrNotGrantable is a grant for a booking this node doesn't hold as
	// its shipper.
	ErrNotGrantable = errors.New("shipper: only a booking's shipper can grant access to it")
	// ErrGrantExpired is a delegated booking whose grant has ended.
	ErrGrantExpired = errors.New("shipper: the delegation for this booking has expired")
)

// GrantTimeout bounds handing over a grant, and subscribing with it.
const GrantTimeout = 15 * time.Second

// Grant issues a delegation for our booking ref with operator to grantee,
// with scope, lasting ttl, signed by this node, and hands it to the
// grantee (ulp.delegation.grant). It records what was granted on the
// booking. The step a person confirms; no model calls it.
func (s *Shipper) Grant(ctx context.Context, operator, ref, grantee string, scope []string, ttl time.Duration, note string) (*store.IssuedGrant, error) {
	self := s.Node.Domain()
	b, err := s.Bookings.Get(operator, ref)
	if err != nil || b.Shipper != self || b.Delegated != nil || b.Order != nil {
		return nil, fmt.Errorf("%w: %s with %s", ErrNotGrantable, ref, operator)
	}
	if b.Cancelled() {
		return nil, fmt.Errorf("%w: %s was cancelled", ErrNotGrantable, ref)
	}
	g, err := delegation.New(self, operator, grantee, ref, scope, s.Now(), ttl)
	if err != nil {
		return nil, err
	}
	body := &intents.DelegationGrantBody{Note: note}
	if err := body.Validate(); err != nil {
		return nil, fmt.Errorf("shipper: %w", err)
	}
	token, err := s.Node.SealDelegation(g)
	if err != nil {
		return nil, err
	}
	h := envelope.NewHeader(intents.DelegationGrant, self, grantee, s.Now())
	h.BookingRef = &ref
	ctx, cancel := context.WithTimeout(node.WithDelegation(ctx, token), GrantTimeout)
	defer cancel()
	if _, err := s.Node.Exchange(ctx, h, body); err != nil {
		return nil, err
	}
	issued := store.IssuedGrant{Grantee: grantee, Scope: g.Scope, TokenID: g.TokenID, Expires: mustTime(g.ExpiresAt), Note: note}
	if _, err := s.Bookings.Update(operator, ref, func(b *store.Booking) error {
		b.Grants = append(b.Grants, issued)
		if len(b.Grants) > store.MaxGrants {
			b.Grants = b.Grants[len(b.Grants)-store.MaxGrants:]
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("shipper: the grant was handed over but not recorded: %w", err)
	}
	return &issued, nil
}

func mustTime(s string) time.Time {
	t, _ := time.Parse(envelope.TimeLayout, s)
	return t
}

// withGrant attaches a delegated booking's token to what ctx sends.
func withGrant(ctx context.Context, b store.Booking, now time.Time) (context.Context, error) {
	if b.Delegated == nil {
		return ctx, nil
	}
	if !b.Delegated.Active(now) {
		return nil, fmt.Errorf("%w: %s, granted by %s", ErrGrantExpired, b.Ref, b.Delegated.Issuer)
	}
	return node.WithDelegation(ctx, b.Delegated.Token), nil
}

// Grants handles ulp.delegation.grant: a booking's shipper hands this node
// a delegation. A grant verified by the pipeline, issued by its sender, to
// this node, for the header's booking, is kept as a delegated booking with
// its carrier (the grant's audience); with tnt:subscribe, Subscribe then
// subscribes to its events (OnGranted runs it, off the request).
type Grants struct {
	Shipper *Shipper
	// From are the issuers whose grants are kept (the companies that ship
	// to us): a grant from anyone else is refused, so nobody can make this
	// node keep a booking, subscribe with a carrier of their choosing, or
	// push to it. Empty: no grant is kept.
	From map[string]bool
	// Endpoint is this node's own push endpoint, for the subscription.
	Endpoint string
	// OnGranted runs for each grant kept, after it is stored. Optional; it
	// must not block (subscribing is a request to the carrier).
	OnGranted func(store.Booking)
}

var grantRefused = node.Reject(intents.CodeForbidden, "This grant can't be accepted.", "")

// Authorize implements node.Handler.
func (g *Grants) Authorize(_ context.Context, req *node.Request) error {
	if req.Delegation == nil {
		return &node.Rejection{Code: grantRefused.Code, Detail: grantRefused.Detail, Reason: "no verified delegation"}
	}
	scope := req.Delegation.Scope()
	if err := req.GrantedHere(scope[0]); err != nil {
		return &node.Rejection{Code: grantRefused.Code, Detail: grantRefused.Detail, Reason: err.Error()}
	}
	issuer, carrier, ref := req.Delegation.Issuer(), req.Delegation.Audience(), req.Delegation.BookingRef()
	if !g.From[issuer] {
		return &node.Rejection{Code: grantRefused.Code, Detail: grantRefused.Detail, Reason: issuer + " is not an issuer this node takes grants from"}
	}
	if carrier == req.Envelope.Header.Receiver {
		return &node.Rejection{Code: grantRefused.Code, Detail: grantRefused.Detail, Reason: "a grant to follow our own booking"}
	}
	if b, err := g.Shipper.Bookings.Get(carrier, ref); err == nil {
		switch {
		case b.Delegated == nil:
			// Ours already, as its shipper: a grant can't stand over it.
			return &node.Rejection{Code: grantRefused.Code, Detail: grantRefused.Detail, Reason: ref + " with " + carrier + " is ours already"}
		case b.Delegated.Issuer != issuer:
			// Followed by another issuer's grant: only it renews it.
			return &node.Rejection{Code: grantRefused.Code, Detail: grantRefused.Detail, Reason: ref + " with " + carrier + " is followed by " + b.Delegated.Issuer + "'s grant"}
		}
	}
	return nil
}

// Handle implements node.Handler: keep (or renew) the delegated booking.
func (g *Grants) Handle(_ context.Context, req *node.Request) (*node.Reply, error) {
	v := req.Delegation
	d := &store.DelegatedBooking{Issuer: v.Issuer(), Token: append([]byte(nil), req.Envelope.Attachments.Delegation...), TokenID: v.TokenID(),
		Scope: v.Scope(), Expires: v.Expires(), Note: req.Body.(*intents.DelegationGrantBody).Note}
	carrier, ref := v.Audience(), v.BookingRef()
	bookings := g.Shipper.Bookings
	renew := func() error {
		_, err := bookings.Update(carrier, ref, func(b *store.Booking) error {
			if b.Delegated == nil || b.Delegated.Issuer != v.Issuer() {
				return errors.New("shipper: the booking changed hands meanwhile")
			}
			d.SubscriptionID, d.Subscribed = b.Delegated.SubscriptionID, b.Delegated.Subscribed
			b.Delegated = d
			return nil
		})
		return err
	}
	if _, err := bookings.Get(carrier, ref); err == nil {
		if err := renew(); err != nil {
			return nil, err
		}
	} else if err := bookings.Add(store.Booking{Ref: ref, Operator: carrier, Shipper: v.Issuer(), Delegated: d, CreatedAt: g.Shipper.Now()}); errors.Is(err, store.ErrExists) {
		// The same grant twice at once: the first one added it.
		if err := renew(); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	b, _ := bookings.Get(carrier, ref)
	if g.OnGranted != nil {
		g.OnGranted(b)
	}
	return nil, nil
}

// Subscribe asks the carrier of a delegated booking to push its events and
// alerts to endpoint, showing the grant, and records the leg it answers
// with. Only a grant with tnt:subscribe can.
func (s *Shipper) Subscribe(ctx context.Context, operator, ref, endpoint string) (*intents.TnTSubscriptionConfirmationBody, error) {
	b, err := s.Bookings.Get(operator, ref)
	if err != nil || b.Delegated == nil {
		return nil, fmt.Errorf("%w: %s with %s (not a delegated booking)", ErrNotOurBooking, ref, operator)
	}
	if !slices.Contains(b.Delegated.Scope, delegation.ScopeSubscribe) {
		return nil, fmt.Errorf("shipper: the grant for %s doesn't include %s", ref, delegation.ScopeSubscribe)
	}
	ctx, err = withGrant(ctx, b, s.Now())
	if err != nil {
		return nil, err
	}
	sub := intents.Subscription{Endpoint: endpoint, Intents: []string{intents.TnTEvent, intents.TnTAlert}}
	h := envelope.NewHeader(intents.TnTSubscriptionRequest, s.Node.Domain(), operator, s.Now())
	h.BookingRef = &ref
	ctx, cancel := context.WithTimeout(ctx, GrantTimeout)
	defer cancel()
	resp, err := s.Node.Exchange(ctx, h, &intents.TnTSubscriptionRequestBody{Subscription: sub}, intents.TnTSubscriptionConfirmation)
	if err != nil {
		return nil, err
	}
	c, ok := resp.Body.(*intents.TnTSubscriptionConfirmationBody)
	if !ok {
		return nil, errors.New("shipper: response is not a subscription confirmation")
	}
	if got := resp.Envelope.Header.BookingRef; got == nil || *got != ref {
		return nil, errors.New("shipper: the answer is about another booking")
	}
	if c.Status != intents.StatusConfirmed {
		return c, nil
	}
	if c.Leg.From == c.Leg.To {
		return nil, errors.New("shipper: the carrier's leg goes nowhere")
	}
	_, err = s.Bookings.Update(operator, ref, func(b *store.Booking) error {
		if b.Delegated == nil {
			return errors.New("shipper: the booking became ours meanwhile")
		}
		b.Delegated.SubscriptionID, b.Delegated.Subscribed = c.SubscriptionID, s.Now()
		legInto(b, *c.Leg, sub)
		return nil
	})
	return c, err
}

// legInto records a delegated booking's leg, as the carrier answered it,
// with our own subscription, where the journey and summary read them.
func legInto(b *store.Booking, l intents.PlannedLeg, sub intents.Subscription) {
	if l.Mode == intents.ModeRoad {
		b.Road = &store.RoadBooking{
			Request: intents.RoadBookingRequestBody{ShipperRef: b.Ref, Shipper: intents.Party{Name: b.Shipper, Domain: b.Shipper},
				Pickup: intents.Stop{UNLocationCode: l.From}, Delivery: intents.Stop{UNLocationCode: l.To}, Subscription: &sub},
			Confirmation: intents.RoadBookingConfirmationBody{BookingRef: b.Ref, Status: intents.StatusConfirmed,
				PlannedPickup: l.PlannedDeparture, PlannedDelivery: l.PlannedArrival},
		}
		return
	}
	b.Request = intents.BookingRequestBody{ShipperRef: b.Ref, ShipperName: b.Shipper, PortOfLoading: l.From, PortOfDischarge: l.To, Subscription: &sub}
	b.Confirmation = intents.BookingConfirmationBody{BookingRef: b.Ref, Status: intents.StatusConfirmed,
		PlannedDeparture: l.PlannedDeparture, PlannedArrival: l.PlannedArrival}
}
