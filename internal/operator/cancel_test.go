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
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
)

func cancellation(sender, ref string) *node.Request {
	h := envelope.NewHeader(intents.BookingCancellation, sender, serra, roadNow)
	h.BookingRef = &ref
	return &node.Request{Envelope: &envelope.Envelope{Header: h}, Body: &intents.BookingCancellationBody{BookingRef: ref}}
}

// cancelService is Serra with one leg booked (SR-BKG-0913) and the
// cancellation service over it.
func cancelService(t *testing.T) (*CancellationService, *RoadAmendmentService, *store.Bookings) {
	t.Helper()
	s, am, bookings := roadServices()
	if _, err := s.Handle(context.Background(), roadRequest(meridian, roadBody())); err != nil {
		t.Fatal(err)
	}
	return &CancellationService{Bookings: bookings, Log: store.NewEventLog(10), Now: func() time.Time { return roadNow }}, am, bookings
}

func cancelAnswer(t *testing.T, c *CancellationService, req *node.Request) *intents.BookingCancellationResponseBody {
	t.Helper()
	if err := c.Authorize(context.Background(), req); err != nil {
		t.Fatalf("authorize: %v", err)
	}
	reply, err := c.Handle(context.Background(), req)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	a := reply.Body.(*intents.BookingCancellationResponseBody)
	if reply.BookingRef != a.BookingRef || a.Validate() != nil {
		t.Fatalf("reply %+v", reply)
	}
	return a
}

// TestCancellation: the leg's shipper cancels it; the shipment stops, the
// booking is kept as cancelled, and asking again gets the same answer.
func TestCancellation(t *testing.T) {
	c, am, bookings := cancelService(t)
	var stopped []string
	var cancelled []store.Booking
	c.Stop = func(ref string) error { stopped = append(stopped, ref); return nil }
	c.OnCancelled = func(b store.Booking) { cancelled = append(cancelled, b) }
	if a := cancelAnswer(t, c, cancellation(meridian, "SR-BKG-0913")); a.Status != intents.StatusCancelled || a.BookingRef != "SR-BKG-0913" {
		t.Fatalf("answer %+v", a)
	}
	b, _ := bookings.Get(serra, "SR-BKG-0913")
	if !b.CancelledAt.Equal(roadNow) || len(stopped) != 1 || len(cancelled) != 1 || !cancelled[0].Cancelled() {
		t.Fatalf("stored %v, stopped %v, cancelled %d", b.CancelledAt, stopped, len(cancelled))
	}
	// Again (its reply lost): the same answer, nothing more done.
	if a := cancelAnswer(t, c, cancellation(meridian, "SR-BKG-0913")); a.Status != intents.StatusCancelled || len(stopped) != 1 || len(cancelled) != 1 {
		t.Errorf("again: %+v, stopped %v", a, stopped)
	}
	// An amendment of a cancelled leg is refused.
	reply, err := am.Handle(context.Background(), amendment(meridian, "SR-BKG-0913", &intents.RoadBookingAmendmentBody{
		Pickup: &intents.Window{Earliest: "2026-10-27T14:00:00Z"}}))
	if err != nil || reply.Body.(*intents.RoadBookingConfirmationBody).Messages[0].Code != "cancelled" {
		t.Errorf("amending a cancelled leg: %+v, %v", reply, err)
	}
	// An ocean booking is cancelled the same way.
	_ = bookings.Add(store.Booking{Ref: "SR-BKG-0001", Shipper: meridian, Operator: serra})
	if a := cancelAnswer(t, c, cancellation(meridian, "SR-BKG-0001")); a.Status != intents.StatusCancelled {
		t.Errorf("ocean: %+v", a)
	}
}

// TestCancellationRefused: only the booking's shipper may cancel it, and
// an unknown booking, an order and someone else's look the same; once the
// cargo has moved, the booking stands.
func TestCancellationRefused(t *testing.T) {
	c, _, bookings := cancelService(t)
	_ = bookings.Add(store.Booking{Ref: "SR-0007", Shipper: meridian, Operator: serra, Order: &store.OrderBooking{}})
	var details []string
	for name, req := range map[string]*node.Request{
		"another sender":     cancellation("mallory.example", "SR-BKG-0913"),
		"an unknown leg":     cancellation(meridian, "SR-BKG-9999"),
		"a forwarding order": cancellation(meridian, "SR-0007"),
		"another's operator": func() *node.Request {
			r := cancellation(meridian, "SR-BKG-0913")
			r.Envelope.Header.Receiver = "other.example"
			return r
		}(),
	} {
		err := c.Authorize(context.Background(), req)
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
	notCancelled := func(name string) {
		t.Helper()
		if b, _ := bookings.Get(serra, "SR-BKG-0913"); b.Cancelled() {
			t.Errorf("%s: cancelled", name)
		}
	}
	// The simulator says the shipment has begun.
	c.Stop = func(string) error { return ErrUnderWay }
	if a := cancelAnswer(t, c, cancellation(meridian, "SR-BKG-0913")); a.Status != intents.StatusRefused || a.Messages[0].Code != "under_way" {
		t.Errorf("begun: %+v", a)
	}
	notCancelled("begun")
	// The simulator fails otherwise: an error, not an answer.
	c.Stop = func(string) error { return errors.New("broken") }
	if _, err := c.Handle(context.Background(), cancellation(meridian, "SR-BKG-0913")); err == nil {
		t.Error("a failing stop answered")
	}
	notCancelled("a failing stop")
	// The carrier recorded an actual event (from its own systems): under way.
	c.Stop = nil
	est, _ := json.Marshal(intents.TnTEventBody{EventID: "EV-1", EventClassifierCode: intents.ClassifierEstimated})
	act, _ := json.Marshal(intents.TnTEventBody{EventID: "EV-2", EventClassifierCode: intents.ClassifierActual})
	_, _ = c.Log.Add(serra, "SR-BKG-0913", store.Record{ID: "EV-1", Intent: intents.TnTEvent, Body: est})
	if moved(c.Log.List(serra, "SR-BKG-0913")) {
		t.Error("an estimate is not a move")
	}
	_, _ = c.Log.Add(serra, "SR-BKG-0913", store.Record{ID: "EV-2", Intent: intents.TnTEvent, Body: act})
	if a := cancelAnswer(t, c, cancellation(meridian, "SR-BKG-0913")); a.Status != intents.StatusRefused {
		t.Errorf("moved: %+v", a)
	}
	notCancelled("moved")
	// Gone between authorizing and handling (a reset).
	if _, err := c.Handle(context.Background(), cancellation(meridian, "SR-BKG-9999")); err == nil {
		t.Error("a booking that's gone answered")
	}
}

// TestCancellationMovedMeanwhile: an actual event recorded after the
// first look, before the cancellation is stored, refuses it; a booking
// cancelled meanwhile is reported once.
func TestCancellationMovedMeanwhile(t *testing.T) {
	c, _, bookings := cancelService(t)
	p := publisher(&fakePusher{}, &drops{})
	t.Cleanup(p.Close)
	ran := 0
	c.Exclusive = func(fn func() error) error { ran++; return p.Exclusive(fn) }
	act, _ := json.Marshal(intents.TnTEventBody{EventID: "EV-2", EventClassifierCode: intents.ClassifierActual})
	c.Stop = func(string) error {
		_, _ = c.Log.Add(serra, "SR-BKG-0913", store.Record{ID: "EV-2", Intent: intents.TnTEvent, Body: act})
		return nil
	}
	if a := cancelAnswer(t, c, cancellation(meridian, "SR-BKG-0913")); a.Status != intents.StatusRefused || ran != 1 {
		t.Errorf("moved meanwhile: %+v (exclusive ran %d)", a, ran)
	}
	if b, _ := bookings.Get(serra, "SR-BKG-0913"); b.Cancelled() {
		t.Error("cancelled after it moved")
	}
	// Cancelled by another cancellation meanwhile: the same answer, told once.
	c2, _, bookings2 := cancelService(t)
	reported := 0
	c2.OnCancelled = func(store.Booking) { reported++ }
	c2.Stop = func(string) error {
		_, err := bookings2.Update(serra, "SR-BKG-0913", func(b *store.Booking) error { b.CancelledAt = roadNow; return nil })
		return err
	}
	if a := cancelAnswer(t, c2, cancellation(meridian, "SR-BKG-0913")); a.Status != intents.StatusCancelled || reported != 0 {
		t.Errorf("cancelled meanwhile: %+v, reported %d", a, reported)
	}
}

// TestPublisherCancelled: nothing is recorded on a cancelled booking, and
// a voyage started after its booking was cancelled ends quietly.
func TestPublisherCancelled(t *testing.T) {
	p := publisher(&fakePusher{}, &drops{})
	t.Cleanup(p.Close)
	bookings := store.NewBookings()
	b := subscribed(intents.TnTEvent)
	b.CancelledAt = roadNow
	_ = bookings.Add(b)
	p.Bookings = bookings
	if err := p.Publish(b, ev("EV-1")); !errors.Is(err, ErrCancelled) || len(p.Log.List(b.Operator, b.Ref)) != 0 {
		t.Errorf("published on a cancelled booking: %v", err)
	}
	s := newTruckSim(t)
	var errs []error
	s.sim.OnError = func(_ string, err error) { errs = append(errs, err) }
	s.sim.Publisher = cancelledPub{}
	for range 40 {
		s.advance(time.Second)
	}
	if s.sim.Active() != 0 || len(errs) != 0 {
		t.Errorf("a cancelled leg's voyage: %d playing, errors %v", s.sim.Active(), errs)
	}
}

// cancelledPub refuses every booking as cancelled.
type cancelledPub struct{}

func (cancelledPub) Publish(store.Booking, intents.Body) error { return ErrCancelled }

// TestCancellationAmendmentRace: an amendment planned while its leg was
// cancelled isn't stored.
func TestCancellationAmendmentRace(t *testing.T) {
	c, am, bookings := cancelService(t)
	am.Backend = racingRoad{trucks(), func() {
		if _, err := c.Handle(context.Background(), cancellation(meridian, "SR-BKG-0913")); err != nil {
			t.Error(err)
		}
	}}
	reply, err := am.Handle(context.Background(), amendment(meridian, "SR-BKG-0913", &intents.RoadBookingAmendmentBody{
		Pickup: &intents.Window{Earliest: "2026-10-25T14:00:00Z"}}))
	if err != nil {
		t.Fatal(err)
	}
	if a := reply.Body.(*intents.RoadBookingConfirmationBody); a.Status != intents.StatusRejected || a.Messages[0].Code != "changed_meanwhile" {
		t.Errorf("reply %+v", a)
	}
	if b, _ := bookings.Get(serra, "SR-BKG-0913"); !b.Cancelled() || b.Road.Request.Pickup.Earliest != "2026-10-24T14:00:00Z" {
		t.Errorf("stored %+v", b.Road.Request.Pickup)
	}
}

// TestSimulatorCancel: a shipment that hasn't begun is ended; one that
// has is under way; one with nothing playing has nothing to end.
func TestSimulatorCancel(t *testing.T) {
	s := newTruckSim(t)
	if err := s.sim.Cancel("SR-BKG-0913"); err != nil || s.sim.Active() != 0 {
		t.Fatalf("before it began: %v, %d playing", err, s.sim.Active())
	}
	for range 40 {
		s.advance(time.Second)
	}
	if got := s.events(); got != "" {
		t.Errorf("a cancelled leg played: %s", got)
	}
	if err := s.sim.Cancel("SR-BKG-9999"); err != nil {
		t.Errorf("nothing playing: %v", err)
	}
	b := newTruckSim(t)
	for range 4 {
		b.advance(time.Second) // past the pick-up
	}
	if err := b.sim.Cancel("SR-BKG-0913"); !errors.Is(err, ErrUnderWay) || b.sim.Active() != 1 {
		t.Errorf("begun: %v, %d playing", err, b.sim.Active())
	}
}

// TestBackendEventCancelled: the carrier's systems can't publish on a
// cancelled booking.
func TestBackendEventCancelled(t *testing.T) {
	p := publisher(&fakePusher{}, &drops{})
	t.Cleanup(p.Close)
	bookings := store.NewBookings()
	b := withContainer(subscribed(intents.TnTEvent), "OCBU0000004")
	b.CancelledAt = roadNow
	_ = bookings.Add(b)
	mux := http.NewServeMux()
	RegisterBackend(mux, p, bookings, p.Log, nil)
	if code, resp := postEvent(t, mux, `{"bookingRef":"OB-BKG-1","intent":"ulp.tnt.event","body":`+eventJSON(t, "EV-9")+`}`); code != http.StatusConflict || resp.Error == "" {
		t.Errorf("%d %+v, want 409", code, resp)
	}
	if got := p.Log.List(b.Operator, b.Ref); len(got) != 0 {
		t.Errorf("recorded %d", len(got))
	}
}
