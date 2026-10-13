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

	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/intents"
)

// truckSim is a simulator with a fake clock behind Serra's road services:
// each leg booked starts, and each amendment re-plans. The leg is picked
// up 2026-10-24 14:00 and delivered 30h later, 2026-10-25 20:00.
type truckSim struct {
	sim      *Simulator
	pub      *published
	book     *RoadBookingService
	amend    *RoadAmendmentService
	bookings *store.Bookings
	now      time.Time
	errs     []error
}

func newTruckSim(t *testing.T) *truckSim {
	t.Helper()
	s := &truckSim{pub: &published{}, now: roadNow}
	s.sim = &Simulator{Publisher: s.pub, HoursPerSecond: 1, StartDelay: time.Second, Now: func() time.Time { return s.now }}
	s.book, s.amend, s.bookings = roadServices()
	s.book.OnBooked = func(b store.Booking) { s.errs = append(s.errs, s.sim.Start(b)) }
	s.amend.OnAmended = func(b store.Booking) { s.errs = append(s.errs, s.sim.Replan(b)) }
	if _, err := s.book.Handle(context.Background(), roadRequest(meridian, roadBody())); err != nil || s.errs[0] != nil {
		t.Fatalf("booked: %v, %v", err, s.errs)
	}
	return s
}

func (s *truckSim) advance(d time.Duration) {
	s.now = s.now.Add(d)
	s.sim.Advance()
}

func (s *truckSim) events() string {
	var out []string
	for _, b := range s.pub.list() {
		out = append(out, summary(b))
	}
	return strings.Join(out, ", ")
}

// TestTruckLeg: a truck leg plays out as pick-up, departure, an estimated
// delivery, the arrival and the drop-off, at its planned times and places.
func TestTruckLeg(t *testing.T) {
	s := newTruckSim(t)
	for range 40 {
		s.advance(time.Second) // 1s delay, 2h lead-in, 30h, 1h to the drop-off
	}
	if got, want := s.events(), "PICK ACT OCBU0000004, DEPA ACT, ARRI EST, ARRI ACT, DROP ACT OCBU0000004"; got != want {
		t.Fatalf("leg:\n got %s\nwant %s", got, want)
	}
	evs := s.pub.list()
	pick, dep, est, arr := evs[0].(*intents.TnTEventBody), evs[1].(*intents.TnTEventBody), evs[2].(*intents.TnTEventBody), evs[3].(*intents.TnTEventBody)
	if pick.EventDateTime != "2026-10-24T14:00:00Z" || pick.UNLocationCode != "BRSSZ" || pick.FacilityName != "Santos Brasil Tecon terminal" ||
		pick.Description != "Container OCBU0000004 picked up at Santos Brasil Tecon terminal" {
		t.Errorf("pick-up %+v", pick)
	}
	if dep.Description != "Truck departed Santos Brasil Tecon terminal for Cymbal Calçados DC, dock 2" || dep.VesselName != "" {
		t.Errorf("departure %+v", dep)
	}
	if est.EventDateTime != "2026-10-25T20:00:00Z" || est.UNLocationCode != "BRSAO" || est.Description != "Truck expected at Cymbal Calçados DC, dock 2" {
		t.Errorf("estimate %+v", est)
	}
	if arr.EventDateTime != "2026-10-25T20:00:00Z" || arr.FacilityName != "Cymbal Calçados DC, dock 2" {
		t.Errorf("arrival %+v", arr)
	}
	b, _ := s.bookings.Get(serra, "SR-BKG-0913")
	if err := s.sim.Replan(b); !errors.Is(err, ErrUnknownBooking) {
		t.Errorf("re-planning a finished leg: %v", err)
	}
}

// TestTruckDelay: a disruption delays the delivery and the drop-off, with
// a new estimate and an alert citing it.
func TestTruckDelay(t *testing.T) {
	s := newTruckSim(t)
	for range 5 {
		s.advance(time.Second) // on the road
	}
	alert, err := s.sim.Disrupt("SR-BKG-0913", 6*time.Hour, "Road closed near Jundiaí.")
	if err != nil {
		t.Fatal(err)
	}
	if alert.NewETA != "2026-10-26T02:00:00Z" || alert.Severity != intents.SeverityLow {
		t.Errorf("alert %+v", alert)
	}
	for range 40 {
		s.advance(time.Second)
	}
	evs := s.pub.list()
	if got := s.events(); got != "PICK ACT OCBU0000004, DEPA ACT, ARRI EST, ARRI EST, alert delay, ARRI ACT, DROP ACT OCBU0000004" {
		t.Fatalf("leg: %s", got)
	}
	if arr := evs[5].(*intents.TnTEventBody); arr.EventDateTime != "2026-10-26T02:00:00Z" {
		t.Errorf("arrival %s", arr.EventDateTime)
	}
}

// TestTruckReplan: an amendment before anything happened plays the leg
// from its new plan; one after the pick-up moves only the delivery, with a
// new estimate; after the arrival, nothing moves.
func TestTruckReplan(t *testing.T) {
	// Before the pick-up: the leg starts over from the 27th.
	s := newTruckSim(t)
	if _, err := s.amend.Handle(context.Background(), amendment(meridian, "SR-BKG-0913", &intents.RoadBookingAmendmentBody{
		Pickup: &intents.Window{Earliest: "2026-10-27T14:00:00Z"}, Delivery: &intents.Window{Latest: "2026-10-29T12:00:00Z"}})); err != nil || s.errs[1] != nil {
		t.Fatalf("amended: %v, %v", err, s.errs)
	}
	for range 40 {
		s.advance(time.Second)
	}
	if pick := s.pub.list()[0].(*intents.TnTEventBody); pick.EventDateTime != "2026-10-27T14:00:00Z" || len(s.pub.list()) != 5 {
		t.Errorf("replayed: %s, %s", pick.EventDateTime, s.events())
	}

	// On the road: only the delivery moves, to the new plan.
	s = newTruckSim(t)
	for range 5 {
		s.advance(time.Second)
	}
	if _, err := s.amend.Handle(context.Background(), amendment(meridian, "SR-BKG-0913", &intents.RoadBookingAmendmentBody{
		Delivery: &intents.Window{Earliest: "2026-10-26T08:00:00Z"}})); err != nil || s.errs[1] != nil {
		t.Fatalf("amended: %v, %v", err, s.errs)
	}
	for range 50 {
		s.advance(time.Second)
	}
	if got := s.events(); got != "PICK ACT OCBU0000004, DEPA ACT, ARRI EST, ARRI EST, ARRI ACT, DROP ACT OCBU0000004" {
		t.Fatalf("leg: %s", got)
	}
	evs := s.pub.list()
	if est := evs[3].(*intents.TnTEventBody); est.EventDateTime != "2026-10-26T08:00:00Z" || !strings.HasSuffix(est.Description, ": new delivery plan") {
		t.Errorf("new estimate %+v", est)
	}
	if arr := evs[4].(*intents.TnTEventBody); arr.EventDateTime != "2026-10-26T08:00:00Z" {
		t.Errorf("arrival %s", arr.EventDateTime)
	}
}

// TestTruckReplanRejects: an ocean booking, an arrived truck and an
// unconfirmed leg aren't re-planned or started.
func TestTruckReplanRejects(t *testing.T) {
	s := newTruckSim(t)
	if err := s.sim.Replan(store.Booking{Ref: "OB-BKG-1"}); err == nil {
		t.Error("re-planned an ocean booking")
	}
	for range 33 {
		s.advance(time.Second) // arrived, not yet dropped off
	}
	b, _ := s.bookings.Get(serra, "SR-BKG-0913")
	if err := s.sim.Replan(b); !errors.Is(err, ErrArrived) {
		t.Errorf("re-planning an arrived truck: %v", err)
	}
	b.Road.Confirmation.Status = intents.StatusRejected
	if err := s.sim.Replan(b); err == nil {
		t.Error("re-planned a rejected leg")
	}
	b.Road.Confirmation.Status, b.Road.Confirmation.PlannedPickup = intents.StatusConfirmed, "soon"
	b.Ref = "SR-BKG-0914"
	if err := s.sim.Start(b); err == nil {
		t.Error("started a leg without planned times")
	}
}

// TestTruckReplanKeepsDelay: a delay announced before an amendment stays
// on top of the new plan, before the pick-up and after it.
func TestTruckReplanKeepsDelay(t *testing.T) {
	for _, onTheRoad := range []bool{false, true} {
		s := newTruckSim(t)
		if onTheRoad {
			for range 5 {
				s.advance(time.Second)
			}
		}
		if _, err := s.sim.Disrupt("SR-BKG-0913", 6*time.Hour, "Road closed near Jundiaí."); err != nil {
			t.Fatal(err)
		}
		// The delivery window moves to the 26th, 08:00 at the earliest: the
		// plan's delivery, then 6 hours late.
		if _, err := s.amend.Handle(context.Background(), amendment(meridian, "SR-BKG-0913", &intents.RoadBookingAmendmentBody{
			Delivery: &intents.Window{Earliest: "2026-10-26T08:00:00Z"}})); err != nil || s.errs[1] != nil {
			t.Fatalf("amended: %v, %v", err, s.errs)
		}
		for range 60 {
			s.advance(time.Second)
		}
		var arrival string
		for _, b := range s.pub.list() {
			if e, ok := b.(*intents.TnTEventBody); ok && e.EventTypeCode == "ARRI" && e.EventClassifierCode == intents.ClassifierActual {
				arrival = e.EventDateTime
			}
		}
		if arrival != "2026-10-26T14:00:00Z" {
			t.Errorf("on the road %v: arrived %s, want the new plan plus the delay; %s", onTheRoad, arrival, s.events())
		}
	}
}

// TestTruckReplanEarlier: a delivery moved earlier than the estimate still
// to come fires before it; a leg planned shorter than the departure's
// offset departs before it arrives.
func TestTruckReplanEarlier(t *testing.T) {
	s := newTruckSim(t)
	s.advance(3 * time.Second) // picked up (2026-10-24 14:00), not yet departed
	b, _ := s.bookings.Get(serra, "SR-BKG-0913")
	b.Road.Confirmation.PlannedDelivery = "2026-10-24T14:20:00Z"
	if err := s.sim.Replan(b); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		s.advance(time.Second)
	}
	// The re-plan's estimate goes out at once; the one planned for later
	// isn't sent after the arrival.
	if got := s.events(); got != "PICK ACT OCBU0000004, ARRI EST, DEPA ACT, ARRI ACT, DROP ACT OCBU0000004" {
		t.Errorf("leg: %s", got)
	}
	short := newTruckSim(t)
	b, _ = short.bookings.Get(serra, "SR-BKG-0913")
	b.Road.Confirmation.PlannedDelivery = b.Road.Confirmation.PlannedPickup
	if err := short.sim.Replan(b); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		short.advance(time.Second)
	}
	if got := short.events(); got != "PICK ACT OCBU0000004, DEPA ACT, ARRI ACT, DROP ACT OCBU0000004" {
		t.Errorf("a leg with no transit: %s", got)
	}
}
