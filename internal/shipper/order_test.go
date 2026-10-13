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
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
)

// The forwarder here is the partner whose key shipperWith knows.
const meridian = "ocean-blue.example"

func anOrder() *intents.ForwardingRequestBody {
	return &intents.ForwardingRequestBody{
		ShipperRef: "CY-PO-55120",
		Incoterms:  intents.Incoterms{Rule: "DAP", Version: "2020", NamedPlace: "Cajamar"},
		Shipper:    intents.Party{Name: "Cymbal Shoes"}, Consignee: intents.Party{Name: "Cymbal Calçados"},
		PlaceOfReceipt:  intents.Place{Type: intents.PlacePort, Facility: "Euromax", UNLocationCode: "NLRTM", Earliest: "2026-10-06T06:00:00Z"},
		PlaceOfDelivery: intents.Place{Type: intents.PlacePort, Facility: "Tecon", UNLocationCode: "BRSSZ", Latest: "2026-10-27T12:00:00Z"},
		Equipment:       []intents.Equipment{{ISOType: "45G1", Count: 1, GrossWeightKg: 12000}},
		Cargo:           intents.Cargo{Description: "Shoes", HSCode: "6404"},
		Subscription:    &intents.Subscription{Endpoint: "https://cymbal-shoes.example/ulp", Intents: []string{intents.TnTEvent}},
	}
}

func journey() intents.ForwardingConfirmationBody {
	return intents.ForwardingConfirmationBody{OrderRef: "MF-0007", Status: intents.StatusConfirmed, SubscriptionID: "SUB-MF-0007",
		PlannedDelivery: "2026-10-23T08:00:00Z",
		Legs:            []intents.PlannedLeg{{Mode: intents.ModeOcean, From: "NLRTM", To: "BRSSZ", PlannedDeparture: "2026-10-07T18:00:00Z"}}}
}

func orderAnswer(c intents.ForwardingConfirmationBody) func(envelope.Header) (string, any) {
	return func(envelope.Header) (string, any) { return intents.ForwardingConfirmation, &c }
}

// TestOrder: an order taken (pending or confirmed) is recorded with the
// message it went in; a rejection isn't; an answer that doesn't fit the
// order, or reuses a reference, is refused.
func TestOrder(t *testing.T) {
	ctx := context.Background()
	pending := intents.ForwardingConfirmationBody{OrderRef: "MF-0007", Status: intents.StatusPending, SubscriptionID: "SUB-MF-0007"}
	for name, answer := range map[string]intents.ForwardingConfirmationBody{"pending": pending, "confirmed": journey()} {
		s := shipperWith(t, orderAnswer(answer))
		c, err := s.Order(ctx, meridian, anOrder())
		if err != nil || c.Status != answer.Status {
			t.Fatalf("%s: %+v, %v", name, c, err)
		}
		b, err := s.Bookings.Get(meridian, "MF-0007")
		if err != nil || b.Order == nil || b.Order.MessageID == "" || b.Order.Confirmation.Status != answer.Status || b.Shipper != "cymbal-shoes.example" {
			t.Errorf("%s: recorded %+v, %v", name, b, err)
		}
		if _, err := s.Order(ctx, meridian, anOrder()); !errors.Is(err, store.ErrExists) {
			t.Errorf("%s: a reused reference: %v", name, err)
		}
	}
	rejected := intents.ForwardingConfirmationBody{OrderRef: "MF-0007", Status: intents.StatusRejected}
	s := shipperWith(t, orderAnswer(rejected))
	if c, err := s.Order(ctx, meridian, anOrder()); err != nil || c.Status != intents.StatusRejected || s.Bookings.Len() != 0 {
		t.Errorf("rejected: %+v, %v", c, err)
	}
	noSub := pending
	noSub.SubscriptionID = ""
	late := journey()
	late.PlannedDelivery = "2026-10-28T08:00:00Z"
	for name, answer := range map[string]intents.ForwardingConfirmationBody{"no subscription": noSub, "a late plan": late} {
		s := shipperWith(t, orderAnswer(answer))
		if _, err := s.Order(ctx, meridian, anOrder()); !errors.Is(err, ErrPlanMismatch) || s.Bookings.Len() != 0 {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A subscription the order didn't ask for.
	unasked := anOrder()
	unasked.Subscription = nil
	s = shipperWith(t, orderAnswer(pending))
	if _, err := s.Order(ctx, meridian, unasked); !errors.Is(err, ErrPlanMismatch) || s.Bookings.Len() != 0 {
		t.Errorf("an unasked-for subscription: %v", err)
	}
	full := shipperWith(t, orderAnswer(pending))
	full.Bookings = store.NewBookingsCap(0)
	if _, err := full.Order(ctx, meridian, anOrder()); err == nil {
		t.Error("recorded into a full store")
	}
	if _, err := (&Shipper{NoBooking: true}).Order(ctx, meridian, anOrder()); !errors.Is(err, ErrNoShipperRole) {
		t.Errorf("a receiver ordering: %v", err)
	}
	refused := shipperWith(t, func(envelope.Header) (string, any) {
		return intents.Error, &intents.ErrorBody{Code: intents.CodeForbidden, Detail: "No."}
	})
	if _, err := refused.Order(ctx, meridian, anOrder()); err == nil {
		t.Error("a refusal")
	}
}

// pendingOrder is Cymbal's record of an order Meridian took as pending.
func pendingOrder() store.Booking {
	return store.Booking{Ref: "MF-0007", Shipper: "cymbal-shoes.example", Operator: meridian,
		Order: &store.OrderBooking{Request: *anOrder(), MessageID: "00000000-0000-4000-8000-000000000001",
			Confirmation: intents.ForwardingConfirmationBody{OrderRef: "MF-0007", Status: intents.StatusPending, SubscriptionID: "SUB-MF-0007"}}}
}

func answerFrom(sender, ref, inReplyTo string, c intents.ForwardingConfirmationBody) *node.Request {
	h := envelope.NewHeader(intents.ForwardingConfirmation, sender, "cymbal-shoes.example", time.Now())
	h.BookingRef, h.InReplyTo = &ref, &inReplyTo
	c.OrderRef = ref
	return &node.Request{Envelope: &envelope.Envelope{Header: h}, Body: &c}
}

// TestOrderAnswers: a forwarder's final answer is taken only from the
// order's forwarder, for an order still pending, in reply to it; anything
// else gets the same refusal.
func TestOrderAnswers(t *testing.T) {
	bookings := store.NewBookings()
	_ = bookings.Add(pendingOrder())
	_ = bookings.Add(store.Booking{Ref: "OB-BKG-1", Operator: meridian}) // an ocean booking
	answered := pendingOrder()
	answered.Ref, answered.Order.Confirmation.Status, answered.Order.Confirmation.OrderRef = "MF-0006", intents.StatusConfirmed, "MF-0006"
	_ = bookings.Add(answered)
	o := &OrderAnswers{Bookings: bookings}
	id := "00000000-0000-4000-8000-000000000001"
	var details []string
	for name, req := range map[string]*node.Request{
		"another forwarder": answerFrom("mallory.example", "MF-0007", id, journey()),
		"an unknown order":  answerFrom(meridian, "MF-9999", id, journey()),
		"an ocean booking":  answerFrom(meridian, "OB-BKG-1", id, journey()),
		"an answered order": answerFrom(meridian, "MF-0006", id, journey()),
		"not a reply to it": answerFrom(meridian, "MF-0007", "00000000-0000-4000-8000-000000000002", journey()),
		"pending once more": answerFrom(meridian, "MF-0007", id, intents.ForwardingConfirmationBody{Status: intents.StatusPending}),
	} {
		err := o.Authorize(context.Background(), req)
		var r *node.Rejection
		if !errors.As(err, &r) || r.Code != intents.CodeForbidden {
			t.Errorf("%s: %v", name, err)
			continue
		}
		details = append(details, r.Detail)
	}
	for _, d := range details {
		if d != details[0] {
			t.Errorf("refusals differ: %q, %q", d, details[0])
		}
	}
	if err := o.Authorize(context.Background(), answerFrom(meridian, "MF-0007", id, journey())); err != nil {
		t.Errorf("the real answer: %v", err)
	}
	// The answer held, sent again (its reply lost): taken, changing nothing.
	again := answerFrom(meridian, "MF-0006", id, answered.Order.Confirmation)
	if err := o.Authorize(context.Background(), again); err != nil {
		t.Errorf("the same answer again: %v", err)
	}
	if _, err := o.Handle(context.Background(), again); err != nil {
		t.Error(err)
	}
	if b, _ := bookings.Get(meridian, "MF-0006"); b.Order.Confirmation.Status != intents.StatusConfirmed {
		t.Error("a resent answer changed the order")
	}
	// The same answer, not in reply to the order: refused.
	forbiddenAgain := answerFrom(meridian, "MF-0006", "00000000-0000-4000-8000-000000000002", answered.Order.Confirmation)
	var r *node.Rejection
	if err := o.Authorize(context.Background(), forbiddenAgain); !errors.As(err, &r) || r.Code != intents.CodeForbidden {
		t.Errorf("the same answer, not in reply: %v", err)
	}
}

// TestOrderAnswersHandle: a plan that fits is recorded, as is a rejection;
// a late plan or a new subscription is refused, and the order stays
// pending.
func TestOrderAnswersHandle(t *testing.T) {
	id := "00000000-0000-4000-8000-000000000001"
	late := journey()
	late.PlannedDelivery = "2026-10-28T08:00:00Z"
	resub := journey()
	resub.SubscriptionID = "SUB-X"
	for name, tc := range map[string]struct {
		c      intents.ForwardingConfirmationBody
		status string
	}{
		"a plan":           {journey(), intents.StatusConfirmed},
		"a rejection":      {intents.ForwardingConfirmationBody{Status: intents.StatusRejected}, intents.StatusRejected},
		"a late plan":      {late, intents.StatusPending},
		"a new subscriber": {resub, intents.StatusPending},
	} {
		bookings := store.NewBookings()
		_ = bookings.Add(pendingOrder())
		var told []store.Booking
		o := &OrderAnswers{Bookings: bookings, OnAnswer: func(b store.Booking) { told = append(told, b) }}
		_, err := o.Handle(context.Background(), answerFrom(meridian, "MF-0007", id, tc.c))
		b, _ := bookings.Get(meridian, "MF-0007")
		if b.Order.Confirmation.Status != tc.status {
			t.Errorf("%s: %s", name, b.Order.Confirmation.Status)
		}
		if refused := tc.status == intents.StatusPending; refused != (err != nil) || refused == (len(told) == 1) {
			t.Errorf("%s: %v, told %d", name, err, len(told))
		}
	}
	// A subscription the order didn't ask for, added in the final answer.
	bookings := store.NewBookings()
	unsubscribed := pendingOrder()
	unsubscribed.Order.Request.Subscription, unsubscribed.Order.Confirmation.SubscriptionID = nil, ""
	_ = bookings.Add(unsubscribed)
	if _, err := (&OrderAnswers{Bookings: bookings}).Handle(context.Background(), answerFrom(meridian, "MF-0007", id, journey())); err == nil {
		t.Error("a subscription added in the final answer")
	}
	// Gone (a reset) between authorizing and handling.
	o := &OrderAnswers{Bookings: store.NewBookings()}
	if _, err := o.Handle(context.Background(), answerFrom(meridian, "MF-0007", id, journey())); err == nil {
		t.Error("handled an order not held")
	}
}
