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

	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
)

const sentID = "0b8b7d5e-3f1a-4c2b-9d1e-5a6b7c8d9e0f"

// statusAnswer answers a status request with st, and anything else (the
// order) with a malformed body: the order's own answer lost on the way.
func statusAnswer(st intents.ForwardingStatusResponseBody) func(envelope.Header) (string, any) {
	return func(h envelope.Header) (string, any) {
		if h.Intent == intents.ForwardingStatusRequest {
			return intents.ForwardingStatusResponse, &st
		}
		return intents.ForwardingConfirmation, &intents.ForwardingConfirmationBody{}
	}
}

func found(id string, answer intents.ForwardingConfirmationBody) intents.ForwardingStatusResponseBody {
	return intents.ForwardingStatusResponseBody{ShipperRef: "CY-PO-55120", OrderRef: answer.OrderRef, OrderMessageID: id, Answer: &answer,
		EstimatedDelivery: answer.PlannedDelivery}
}

// TestOrderStatusAdopts: an order sent whose first answer never arrived is
// taken up when the forwarder's status names the message it went in; the
// forwarder's later answers then reply to it as to any order.
func TestOrderStatusAdopts(t *testing.T) {
	ctx := context.Background()
	pending := intents.ForwardingConfirmationBody{OrderRef: "MF-0007", Status: intents.StatusPending, SubscriptionID: "SUB-MF-0007"}
	s := shipperWith(t, statusAnswer(found(sentID, pending)))
	s.rememberSent(sentID, meridian, anOrder())
	st, err := s.OrderStatus(ctx, meridian, "CY-PO-55120")
	if err != nil || !st.Adopted || st.Held {
		t.Fatalf("status %+v, %v", st, err)
	}
	b, err := s.Bookings.Get(meridian, "MF-0007")
	if err != nil || b.Order.MessageID != sentID || b.Order.Confirmation.Status != intents.StatusPending || b.Order.Request.ShipperRef != "CY-PO-55120" {
		t.Fatalf("recorded %+v, %v", b, err)
	}
	if _, kept := s.sentTo(sentID, meridian); kept {
		t.Error("still awaiting a first answer")
	}
	// Asked again: held, nothing changes.
	if st, err := s.OrderStatus(ctx, meridian, "CY-PO-55120"); err != nil || !st.Held || st.Adopted {
		t.Errorf("again: %+v, %v", st, err)
	}
	// A confirmed answer, as the status reports it, is held to the order.
	c := shipperWith(t, statusAnswer(found(sentID, journey())))
	c.rememberSent(sentID, meridian, anOrder())
	if st, err := c.OrderStatus(ctx, meridian, "CY-PO-55120"); err != nil || !st.Adopted {
		t.Errorf("confirmed: %+v, %v", st, err)
	}
}

// TestOrderStatusRefuses: what the shipper didn't send, or doesn't fit
// what it sent, isn't taken up; an answer about something else is refused.
func TestOrderStatusRefuses(t *testing.T) {
	ctx := context.Background()
	pending := intents.ForwardingConfirmationBody{OrderRef: "MF-0007", Status: intents.StatusPending, SubscriptionID: "SUB-MF-0007"}
	// No record of sending it (or another message's ID): unknown, not taken.
	for name, id := range map[string]string{"never sent": "", "another message": "1c9c8e6f-4a2b-4d3c-8e2f-6b7c8d9e0f1a"} {
		s := shipperWith(t, statusAnswer(found("1c9c8e6f-4a2b-4d3c-8e2f-6b7c8d9e0f1a", pending)))
		if id == "another message" {
			s.rememberSent(sentID, meridian, anOrder())
		}
		if st, err := s.OrderStatus(ctx, meridian, "CY-PO-55120"); err != nil || !st.Unknown || s.Bookings.Len() != 0 {
			t.Errorf("%s: %+v, %v", name, st, err)
		}
	}
	// Sent to another forwarder: not this one's to answer for.
	s := shipperWith(t, statusAnswer(found(sentID, pending)))
	s.rememberSent(sentID, "other-forwarder.example", anOrder())
	if st, err := s.OrderStatus(ctx, meridian, "CY-PO-55120"); err != nil || !st.Unknown || s.Bookings.Len() != 0 {
		t.Errorf("another forwarder's: %+v, %v", st, err)
	}
	// An answer that doesn't fit the order sent: refused, not taken up.
	late := journey()
	late.PlannedDelivery = "2026-10-28T08:00:00Z"
	s = shipperWith(t, statusAnswer(found(sentID, late)))
	s.rememberSent(sentID, meridian, anOrder())
	if _, err := s.OrderStatus(ctx, meridian, "CY-PO-55120"); !errors.Is(err, ErrPlanMismatch) || s.Bookings.Len() != 0 {
		t.Errorf("a plan that doesn't fit: %v", err)
	}
	// About another reference: refused.
	other := found(sentID, pending)
	other.ShipperRef = "CY-PO-99999"
	s = shipperWith(t, statusAnswer(other))
	if _, err := s.OrderStatus(ctx, meridian, "CY-PO-55120"); !errors.Is(err, ErrStatusMismatch) {
		t.Errorf("another reference: %v", err)
	}
	// Held under that reference, but another order: refused.
	s = shipperWith(t, statusAnswer(found(sentID, pending)))
	_ = s.Bookings.Add(store.Booking{Ref: "MF-0007", Operator: meridian, Shipper: "cymbal-shoes.example",
		Order: &store.OrderBooking{Request: intents.ForwardingRequestBody{ShipperRef: "CY-PO-11111"}}})
	if _, err := s.OrderStatus(ctx, meridian, "CY-PO-55120"); !errors.Is(err, ErrStatusMismatch) {
		t.Errorf("another order under the reference: %v", err)
	}
	// Rejected: nothing to take up, and the order is no longer awaited.
	rejected := intents.ForwardingConfirmationBody{OrderRef: "MF-0007", Status: intents.StatusRejected}
	s = shipperWith(t, statusAnswer(found(sentID, rejected)))
	s.rememberSent(sentID, meridian, anOrder())
	if st, err := s.OrderStatus(ctx, meridian, "CY-PO-55120"); err != nil || st.Adopted || s.Bookings.Len() != 0 {
		t.Errorf("rejected: %+v, %v", st, err)
	}
	if _, kept := s.sentTo(sentID, meridian); kept {
		t.Error("a rejected order still awaited")
	}
	// No such order.
	s = shipperWith(t, statusAnswer(intents.ForwardingStatusResponseBody{ShipperRef: "CY-PO-55120"}))
	if st, err := s.OrderStatus(ctx, meridian, "CY-PO-55120"); err != nil || st.Status.Found() || st.Adopted {
		t.Errorf("not found: %+v, %v", st, err)
	}
	// A reference that isn't one: not sent.
	if _, err := s.OrderStatus(ctx, meridian, "a/b"); err == nil {
		t.Error("asked about a reference that isn't one")
	}
}

// TestOrderKeepsUnanswered: an order whose answer never arrived is kept
// for a status request; one answered and recorded isn't; the oldest are
// forgotten past MaxSentOrders.
func TestOrderKeepsUnanswered(t *testing.T) {
	ctx := context.Background()
	s := shipperWith(t, statusAnswer(intents.ForwardingStatusResponseBody{ShipperRef: "CY-PO-55120"}))
	if _, err := s.Order(ctx, meridian, anOrder()); err == nil {
		t.Fatal("a malformed answer taken")
	}
	if len(s.sent) != 1 {
		t.Errorf("kept %d unanswered orders, want 1", len(s.sent))
	}
	pending := intents.ForwardingConfirmationBody{OrderRef: "MF-0007", Status: intents.StatusPending, SubscriptionID: "SUB-MF-0007"}
	ok := shipperWith(t, orderAnswer(pending))
	if _, err := ok.Order(ctx, meridian, anOrder()); err != nil || len(ok.sent) != 0 {
		t.Errorf("an answered order still kept: %d, %v", len(ok.sent), err)
	}
	for i := range MaxSentOrders + 5 {
		s.rememberSent(string(rune('a'+i%26))+string(rune(i)), meridian, anOrder())
	}
	if len(s.sent) > MaxSentOrders || len(s.sentOrder) > MaxSentOrders {
		t.Errorf("kept %d, over %d", len(s.sent), MaxSentOrders)
	}
}

// TestOrderStatusErrors: an answer that isn't a status, or whose header
// names no order while its body names one, is refused.
func TestOrderStatusErrors(t *testing.T) {
	ctx := context.Background()
	s := shipperWith(t, func(envelope.Header) (string, any) {
		return intents.ForwardingConfirmation, &intents.ForwardingConfirmationBody{}
	})
	if _, err := s.OrderStatus(ctx, meridian, "CY-PO-55120"); err == nil {
		t.Error("an answer of another intent taken")
	}
	// The same body, but the header names no booking.
	pending := intents.ForwardingConfirmationBody{OrderRef: "MF-0007", Status: intents.StatusPending, SubscriptionID: "SUB-MF-0007"}
	st := found(sentID, pending)
	s = shipperWith(t, func(envelope.Header) (string, any) {
		return intents.ForwardingStatusResponse, struct {
			*intents.ForwardingStatusResponseBody
		}{&st}
	})
	s.rememberSent(sentID, meridian, anOrder())
	if _, err := s.OrderStatus(ctx, meridian, "CY-PO-55120"); err == nil || s.Bookings.Len() != 0 {
		t.Errorf("a header naming no order: %v", err)
	}
}

// TestOrderStatusRace: an order taken up by a status request while its
// first answer was on the way is the order's answer, not "taken again";
// a held order sent in another message isn't ours.
func TestOrderStatusRace(t *testing.T) {
	ctx := context.Background()
	pending := intents.ForwardingConfirmationBody{OrderRef: "MF-0007", Status: intents.StatusPending, SubscriptionID: "SUB-MF-0007"}
	var s *Shipper
	s = shipperWith(t, func(h envelope.Header) (string, any) {
		// A status request took the order up before this answer arrived.
		_ = s.Bookings.Add(store.Booking{Ref: "MF-0007", Operator: meridian, Shipper: "cymbal-shoes.example",
			Order: &store.OrderBooking{Request: *anOrder(), MessageID: h.MessageID, Confirmation: pending}})
		return intents.ForwardingConfirmation, &pending
	})
	if c, err := s.Order(ctx, meridian, anOrder()); err != nil || c.OrderRef != "MF-0007" || len(s.sent) != 0 {
		t.Errorf("taken up meanwhile: %+v, %v (kept %d)", c, err, len(s.sent))
	}
	// Held under the reference, but sent in another message.
	s = shipperWith(t, statusAnswer(found(sentID, pending)))
	_ = s.Bookings.Add(store.Booking{Ref: "MF-0007", Operator: meridian, Shipper: "cymbal-shoes.example",
		Order: &store.OrderBooking{Request: *anOrder(), MessageID: "1c9c8e6f-4a2b-4d3c-8e2f-6b7c8d9e0f1a", Confirmation: pending}})
	if _, err := s.OrderStatus(ctx, meridian, "CY-PO-55120"); !errors.Is(err, ErrStatusMismatch) {
		t.Errorf("held, another message: %v", err)
	}
	if s.heldOrder(meridian, "MF-0007", sentID) {
		t.Error("another message's order held as this one")
	}
}
