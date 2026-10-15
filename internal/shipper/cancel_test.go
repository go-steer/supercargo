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
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
)

const ob = "ocean-blue.example"

// cancelAnswer answers a cancellation with status (and a reason if
// refused), counting the cancellations sent.
func cancelAnswer(status string, sent *int) func(envelope.Header) (string, any) {
	return func(h envelope.Header) (string, any) {
		*sent++
		a := &intents.BookingCancellationResponseBody{BookingRef: *h.BookingRef, Status: status}
		if status == intents.StatusRefused {
			a.Messages = []intents.Message{{Code: "under_way", Detail: "The cargo has moved."}}
		}
		return intents.BookingCancellationResponse, a
	}
}

func withLeg(s *Shipper) *Shipper {
	_ = s.Bookings.Add(store.Booking{Ref: "OB-BKG-R-0913", Shipper: "cymbal-shoes.example", Operator: ob,
		Road: &store.RoadBooking{Request: *leg(), Confirmation: planned}})
	return s
}

// TestCancel: a cancelled answer is recorded; the booking then takes no
// pushes, can't be amended, and isn't cancelled twice.
func TestCancel(t *testing.T) {
	ctx := context.Background()
	sent := 0
	s := withLeg(shipperWith(t, cancelAnswer(intents.StatusCancelled, &sent)))
	a, err := s.Cancel(ctx, ob, "OB-BKG-R-0913")
	if err != nil || a.Status != intents.StatusCancelled || sent != 1 {
		t.Fatalf("cancel: %+v, %v", a, err)
	}
	b, _ := s.Bookings.Get(ob, "OB-BKG-R-0913")
	if !b.Cancelled() {
		t.Fatal("not recorded as cancelled")
	}
	if _, err := s.Cancel(ctx, ob, "OB-BKG-R-0913"); !errors.Is(err, ErrCancelled) || sent != 1 {
		t.Errorf("twice: %v, sent %d", err, sent)
	}
	if _, err := s.AmendRoad(ctx, ob, "OB-BKG-R-0913", &intents.RoadBookingAmendmentBody{Pickup: &intents.Window{Latest: "2026-10-24T20:00:00Z"}}); !errors.Is(err, ErrCancelled) {
		t.Errorf("amending a cancelled leg: %v", err)
	}
	// An ocean booking is cancelled the same way.
	_ = s.Bookings.Add(store.Booking{Ref: "OB-BKG-1", Shipper: "cymbal-shoes.example", Operator: ob})
	if a, err := s.Cancel(ctx, ob, "OB-BKG-1"); err != nil || a.Status != intents.StatusCancelled {
		t.Errorf("ocean: %+v, %v", a, err)
	}
}

// TestCancelRefused: a refusal changes nothing; nothing is sent for a
// booking we don't hold or an order, and an answer of another kind is an
// error.
func TestCancelRefused(t *testing.T) {
	ctx := context.Background()
	sent := 0
	s := withLeg(shipperWith(t, cancelAnswer(intents.StatusRefused, &sent)))
	a, err := s.Cancel(ctx, ob, "OB-BKG-R-0913")
	if err != nil || a.Status != intents.StatusRefused || a.Messages[0].Code != "under_way" {
		t.Fatalf("refused: %+v, %v", a, err)
	}
	if b, _ := s.Bookings.Get(ob, "OB-BKG-R-0913"); b.Cancelled() {
		t.Error("a refusal recorded as cancelled")
	}
	_ = s.Bookings.Add(store.Booking{Ref: "OB-0007", Operator: ob, Order: &store.OrderBooking{}})
	for name, ref := range map[string]string{"unknown": "OB-BKG-9", "an order": "OB-0007"} {
		if _, err := s.Cancel(ctx, ob, ref); !errors.Is(err, ErrNotOurBooking) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if sent != 1 {
		t.Errorf("sent %d, want 1", sent)
	}
	e := withLeg(shipperWith(t, func(envelope.Header) (string, any) {
		return intents.Error, &intents.ErrorBody{Code: intents.CodeForbidden, Detail: "No."}
	}))
	if _, err := e.Cancel(ctx, ob, "OB-BKG-R-0913"); err == nil {
		t.Error("an error answer taken")
	}
	if b, _ := e.Bookings.Get(ob, "OB-BKG-R-0913"); b.Cancelled() {
		t.Error("an error recorded as cancelled")
	}
}

// TestCancelOtherAnswer: an answer naming another booking isn't this
// one's, and a leg of an order is the order's to cancel.
func TestCancelOtherAnswer(t *testing.T) {
	ctx := context.Background()
	s := withLeg(shipperWith(t, func(envelope.Header) (string, any) {
		return intents.BookingCancellationResponse, &intents.BookingCancellationResponseBody{BookingRef: "OB-BKG-2", Status: intents.StatusCancelled}
	}))
	if _, err := s.Cancel(ctx, ob, "OB-BKG-R-0913"); !errors.Is(err, ErrPlanMismatch) {
		t.Errorf("an answer about another: %v", err)
	}
	if b, _ := s.Bookings.Get(ob, "OB-BKG-R-0913"); b.Cancelled() {
		t.Error("recorded as cancelled")
	}
	sent := 0
	o := withLeg(shipperWith(t, cancelAnswer(intents.StatusCancelled, &sent)))
	o.InOrder = func(carrier, ref string) bool { return carrier == ob && ref == "OB-BKG-R-0913" }
	if _, err := o.Cancel(ctx, ob, "OB-BKG-R-0913"); !errors.Is(err, ErrLegOfOrder) || sent != 0 {
		t.Errorf("a leg of an order: %v, sent %d", err, sent)
	}
	if code, _ := postCancel(t, ControlHandler(o, nil), ob, "OB-BKG-R-0913"); code != http.StatusConflict {
		t.Errorf("a leg of an order: HTTP %d", code)
	}
}

// TestInboundCancelled: a cancelled booking's subscription is over.
func TestInboundCancelled(t *testing.T) {
	in, _ := inbound(t, intents.TnTEvent)
	_, _ = in.Bookings.Update(ob, "OB-BKG-1", func(b *store.Booking) error { b.CancelledAt = time.Now(); return nil })
	var r *node.Rejection
	if err := in.Authorize(context.Background(), pushReq(ob, "OB-BKG-1", anEvent)); !errors.As(err, &r) || r.Code != intents.CodeForbidden {
		t.Errorf("a push to a cancelled booking: %v", err)
	}
	alerts, _ := inbound(t, intents.TnTAlert)
	_, _ = alerts.Bookings.Update(ob, "OB-BKG-1", func(b *store.Booking) error { b.CancelledAt = time.Now(); return nil })
	if err := alerts.Authorize(context.Background(), pushReq(ob, "OB-BKG-1", anAlert)); err == nil {
		t.Error("an alert to a cancelled booking")
	}
}

// TestAmendRoadCancelledMeanwhile: a leg cancelled while its amendment
// was out keeps no new plan.
func TestAmendRoadCancelledMeanwhile(t *testing.T) {
	var s *Shipper
	s = withLeg(shipperWith(t, func(h envelope.Header) (string, any) {
		_, _ = s.Bookings.Update(ob, "OB-BKG-R-0913", func(b *store.Booking) error { b.CancelledAt = time.Now(); return nil })
		c := planned
		c.PlannedPickup = "2026-10-24T18:00:00Z"
		return intents.RoadBookingConfirmation, &c
	}))
	if _, err := s.AmendRoad(context.Background(), ob, "OB-BKG-R-0913", &intents.RoadBookingAmendmentBody{
		Pickup: &intents.Window{Latest: "2026-10-24T20:00:00Z"}}); !errors.Is(err, ErrLegChanged) {
		t.Errorf("amended a leg cancelled meanwhile: %v", err)
	}
}

func postCancel(t *testing.T, h http.Handler, operator, ref string) (int, CancelResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/shipments/"+operator+"/"+ref+"/cancel", nil))
	var resp CancelResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec.Code, resp
}

// TestControlCancel: POST /shipments/{operator}/{ref}/cancel cancels;
// what can't be is said, by status.
func TestControlCancel(t *testing.T) {
	sent := 0
	s := withLeg(shipperWith(t, cancelAnswer(intents.StatusCancelled, &sent)))
	h := ControlHandler(s, nil)
	if code, resp := postCancel(t, h, ob, "OB-BKG-R-0913"); code != http.StatusOK || resp.Answer == nil || resp.Answer.Status != intents.StatusCancelled {
		t.Fatalf("cancel: HTTP %d %+v", code, resp)
	}
	if code, resp := postCancel(t, h, ob, "OB-BKG-R-0913"); code != http.StatusConflict || resp.Error == "" {
		t.Errorf("twice: HTTP %d %+v", code, resp)
	}
	if code, resp := postCancel(t, h, ob, "OB-BKG-9"); code != http.StatusNotFound || resp.Error == "" {
		t.Errorf("unknown: HTTP %d %+v", code, resp)
	}
	e := withLeg(shipperWith(t, func(envelope.Header) (string, any) {
		return intents.Error, &intents.ErrorBody{Code: intents.CodeForbidden, Detail: "No."}
	}))
	if code, resp := postCancel(t, ControlHandler(e, nil), ob, "OB-BKG-R-0913"); code != http.StatusBadGateway || resp.Error == "" {
		t.Errorf("refused by the node: HTTP %d %+v", code, resp)
	}
}
