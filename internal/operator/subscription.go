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
	"slices"
	"time"

	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/delegation"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
)

// SubscriptionService handles ulp.tnt.subscription.request (docs/DESIGN.md,
// A.5 "Delegated access"): a company the booking's shipper delegated
// tnt:subscribe to subscribes its own endpoint to the booking's events and
// alerts, until the grant expires. It never learns more of the booking
// than the leg's places and times.
type SubscriptionService struct {
	Bookings *store.Bookings
	Now      func() time.Time
	// MaxDelegates bounds a booking's delegated subscriptions (default 8).
	MaxDelegates int
	// OnSubscribed runs for each subscription confirmed. Optional.
	OnSubscribed func(b store.Booking, d store.Delegate)
}

// Authorize implements node.Handler: the booking exists and stands, the
// delegation is its shipper's, to this carrier, for the sender, the
// booking and tnt:subscribe, and the endpoint is on the sender's own
// domain.
func (s *SubscriptionService) Authorize(_ context.Context, req *node.Request) error {
	h := req.Envelope.Header
	b, err := s.Bookings.Get(h.Receiver, *h.BookingRef)
	if err != nil {
		return &node.Rejection{Code: notAParty.Code, Detail: notAParty.Detail, Reason: "no booking " + *h.BookingRef}
	}
	if err := req.Delegated(b.Shipper, delegation.ScopeSubscribe); err != nil { // nil without a verified one
		return &node.Rejection{Code: notAParty.Code, Detail: notAParty.Detail, Reason: err.Error()}
	}
	if b.Cancelled() {
		return &node.Rejection{Code: notAParty.Code, Detail: notAParty.Detail, Reason: b.Ref + " was cancelled"}
	}
	if b.Order != nil {
		// A forwarding order's journey is its legs': a delegate follows a leg.
		return &node.Rejection{Code: notAParty.Code, Detail: notAParty.Detail, Reason: b.Ref + " is a forwarding order"}
	}
	sub := req.Body.(*intents.TnTSubscriptionRequestBody).Subscription
	if sub.EndpointHost() != h.Sender {
		return node.Reject(intents.CodeForbidden, "The subscription endpoint must be on the sender's own domain.",
			"subscription endpoint host "+sub.EndpointHost()+" is not "+h.Sender)
	}
	return nil
}

// Handle implements node.Handler: the grantee's subscription replaces any
// it had for the booking, and the answer names the leg as planned.
func (s *SubscriptionService) Handle(_ context.Context, req *node.Request) (*node.Reply, error) {
	h := req.Envelope.Header
	sub := req.Body.(*intents.TnTSubscriptionRequestBody).Subscription
	max := s.MaxDelegates
	if max == 0 {
		max = 8
	}
	d := store.Delegate{Grantee: h.Sender, Subscription: sub, TokenID: req.Delegation.TokenID(), Expires: req.Delegation.Expires(),
		SubscriptionID: fmt.Sprintf("SUB-D-%s-%s", *h.BookingRef, shortID(req.Delegation.TokenID()))}
	errFull := errors.New("full")
	b, err := s.Bookings.Update(h.Receiver, *h.BookingRef, func(b *store.Booking) error {
		now := s.Now()
		kept := slices.DeleteFunc(b.Delegates, func(x store.Delegate) bool { return x.Grantee == h.Sender || !x.Active(now) })
		if len(kept) >= max {
			return errFull
		}
		kept = append(kept, d)
		b.Delegates = kept
		return nil
	})
	if errors.Is(err, errFull) {
		return &node.Reply{Body: &intents.TnTSubscriptionConfirmationBody{Status: intents.StatusRejected,
			Messages: []intents.Message{{Code: "too_many_subscriptions", Detail: "This booking has as many delegated subscriptions as the carrier keeps."}}}}, nil
	}
	if err != nil {
		return nil, err
	}
	if s.OnSubscribed != nil {
		s.OnSubscribed(b, d)
	}
	return &node.Reply{Body: &intents.TnTSubscriptionConfirmationBody{Status: intents.StatusConfirmed, SubscriptionID: d.SubscriptionID,
		ExpiresAt: d.Expires.UTC().Format(envelope.TimeLayout), Leg: legOf(b)}}, nil
}

// shortID is the start of a token ID, for a subscription's ID.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// legOf is a booking's leg as the carrier plans it: its places and times.
func legOf(b store.Booking) *intents.PlannedLeg {
	if r := b.Road; r != nil {
		return &intents.PlannedLeg{Mode: intents.ModeRoad, From: r.Request.Pickup.UNLocationCode, To: r.Request.Delivery.UNLocationCode,
			PlannedDeparture: r.Confirmation.PlannedPickup, PlannedArrival: r.Confirmation.PlannedDelivery}
	}
	c := b.Confirmation
	return &intents.PlannedLeg{Mode: intents.ModeOcean, From: b.Request.PortOfLoading, To: b.Request.PortOfDischarge,
		PlannedDeparture: c.PlannedDeparture, PlannedArrival: c.PlannedArrival}
}
