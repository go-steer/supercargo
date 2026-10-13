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
	"strings"
	"testing"

	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
)

func leg() *intents.RoadBookingRequestBody {
	return &intents.RoadBookingRequestBody{
		ShipperRef: "CY-L3", Shipper: intents.Party{Name: "Cymbal Shoes"},
		Pickup:    intents.Stop{Facility: "Tecon", UNLocationCode: "BRSSZ", Earliest: "2026-10-24T14:00:00Z"},
		Delivery:  intents.Stop{Facility: "DC", UNLocationCode: "BRSAO", Latest: "2026-10-27T12:00:00Z"},
		Equipment: []intents.Equipment{{ISOType: "45G1", Count: 1, GrossWeightKg: 12000}},
		Cargo:     intents.Cargo{Description: "Shoes", HSCode: "6404"},
	}
}

func roadAnswer(c intents.RoadBookingConfirmationBody) func(envelope.Header) (string, any) {
	return func(envelope.Header) (string, any) { return intents.RoadBookingConfirmation, &c }
}

var planned = intents.RoadBookingConfirmationBody{BookingRef: "OB-BKG-R-0913", Status: intents.StatusConfirmed,
	PlannedPickup: "2026-10-24T14:00:00Z", PlannedDelivery: "2026-10-25T20:00:00Z"}

// TestBookRoad: a confirmed leg is recorded; a rejection, a plan outside
// the windows and a reused reference are not.
func TestBookRoad(t *testing.T) {
	ctx := context.Background()
	s := shipperWith(t, roadAnswer(planned))
	c, err := s.BookRoad(ctx, "ocean-blue.example", leg())
	if err != nil || c.BookingRef != "OB-BKG-R-0913" {
		t.Fatalf("%+v, %v", c, err)
	}
	if b, err := s.Bookings.Get("ocean-blue.example", "OB-BKG-R-0913"); err != nil || b.Road == nil || b.Shipper != "cymbal-shoes.example" {
		t.Fatalf("recorded %+v, %v", b, err)
	}
	if _, err := s.BookRoad(ctx, "ocean-blue.example", leg()); !errors.Is(err, store.ErrExists) {
		t.Errorf("a reused reference: %v", err)
	}
	rejected := intents.RoadBookingConfirmationBody{BookingRef: "OB-BKG-R-0914", Status: intents.StatusRejected}
	s = shipperWith(t, roadAnswer(rejected))
	if c, err := s.BookRoad(ctx, "ocean-blue.example", leg()); err != nil || c.Status != intents.StatusRejected || s.Bookings.Len() != 0 {
		t.Errorf("rejected: %+v, %v, %d recorded", c, err, s.Bookings.Len())
	}
	early := planned
	early.PlannedPickup = "2026-10-24T08:00:00Z"
	s = shipperWith(t, roadAnswer(early))
	if _, err := s.BookRoad(ctx, "ocean-blue.example", leg()); !errors.Is(err, ErrPlanMismatch) || s.Bookings.Len() != 0 {
		t.Errorf("a pickup before the window: %v", err)
	}
	full := shipperWith(t, roadAnswer(planned))
	full.Bookings = store.NewBookingsCap(0)
	if _, err := full.BookRoad(ctx, "ocean-blue.example", leg()); err == nil {
		t.Error("recorded into a full store")
	}
}

// TestAmendRoad: a confirmed new plan replaces the recorded one; a
// rejection, a plan outside the new windows or a changed subscription
// leave the leg as it was.
func TestAmendRoad(t *testing.T) {
	ctx := context.Background()
	moved := planned
	moved.PlannedPickup, moved.PlannedDelivery = "2026-10-27T14:00:00Z", "2026-10-28T20:00:00Z"
	resub := moved
	resub.SubscriptionID = "SUB-X"
	for name, tc := range map[string]struct {
		answer intents.RoadBookingConfirmationBody
		ok     bool
		want   error
	}{
		"confirmed":            {moved, true, nil},
		"rejected":             {intents.RoadBookingConfirmationBody{BookingRef: "OB-BKG-R-0913", Status: intents.StatusRejected}, false, nil},
		"outside the window":   {planned, false, ErrPlanMismatch}, // the old pickup, before the new window
		"another subscription": {resub, false, ErrPlanMismatch},
		"another leg":          {func() intents.RoadBookingConfirmationBody { m := moved; m.BookingRef = "OB-BKG-R-0914"; return m }(), false, ErrPlanMismatch},
	} {
		s := shipperWith(t, roadAnswer(tc.answer))
		_ = s.Bookings.Add(store.Booking{Ref: "OB-BKG-R-0913", Shipper: "cymbal-shoes.example", Operator: "ocean-blue.example",
			Road: &store.RoadBooking{Request: *leg(), Confirmation: planned}})
		_, err := s.AmendRoad(ctx, "ocean-blue.example", "OB-BKG-R-0913", &intents.RoadBookingAmendmentBody{
			Pickup: &intents.Window{Earliest: "2026-10-27T14:00:00Z"}, Delivery: &intents.Window{Latest: "2026-10-29T12:00:00Z"}})
		if tc.want != nil && !errors.Is(err, tc.want) || tc.want == nil && err != nil {
			t.Errorf("%s: %v", name, err)
		}
		b, _ := s.Bookings.Get("ocean-blue.example", "OB-BKG-R-0913")
		if changed := b.Road.Confirmation.PlannedPickup != planned.PlannedPickup; changed != tc.ok {
			t.Errorf("%s: changed %v", name, changed)
		}
		if tc.ok && (b.Road.Request.Pickup.Earliest != "2026-10-27T14:00:00Z" || b.Road.Request.Delivery.Latest != "2026-10-29T12:00:00Z") {
			t.Errorf("%s: windows %+v", name, b.Road.Request)
		}
	}
}

// TestCheckPlan: a carrier's plan outside the windows asked for, or
// without the subscription asked for, is refused, each for its reason.
func TestCheckPlan(t *testing.T) {
	req := &intents.RoadBookingRequestBody{
		Pickup:       intents.Stop{Earliest: "2026-10-24T14:00:00Z", Latest: "2026-10-24T20:00:00Z"},
		Delivery:     intents.Stop{Earliest: "2026-10-25T08:00:00Z", Latest: "2026-10-27T12:00:00Z"},
		Subscription: &intents.Subscription{Endpoint: "https://meridian-forwarding.example/ulp"},
	}
	ok := intents.RoadBookingConfirmationBody{PlannedPickup: "2026-10-24T14:00:00Z", PlannedDelivery: "2026-10-25T20:00:00Z", SubscriptionID: "SUB-1"}
	if err := checkPlan(req, &ok); err != nil {
		t.Fatal(err)
	}
	for want, edit := range map[string]func(*intents.RoadBookingConfirmationBody){
		"planned_pickup 2026-10-24T13:00:00Z is before":   func(c *intents.RoadBookingConfirmationBody) { c.PlannedPickup = "2026-10-24T13:00:00Z" },
		"planned_pickup 2026-10-24T21:00:00Z is after":    func(c *intents.RoadBookingConfirmationBody) { c.PlannedPickup = "2026-10-24T21:00:00Z" },
		"planned_delivery 2026-10-25T07:00:00Z is before": func(c *intents.RoadBookingConfirmationBody) { c.PlannedDelivery = "2026-10-25T07:00:00Z" },
		"planned_delivery 2026-10-28T07:00:00Z is after":  func(c *intents.RoadBookingConfirmationBody) { c.PlannedDelivery = "2026-10-28T07:00:00Z" },
		"no subscription_id":                              func(c *intents.RoadBookingConfirmationBody) { c.SubscriptionID = "" },
	} {
		c := ok
		edit(&c)
		if err := checkPlan(req, &c); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", want, err)
		}
	}
}

// TestRoadNoBooking: a receiver-only node can't book a truck leg, and no
// node amends a leg it doesn't hold (or an ocean booking).
func TestRoadNoBooking(t *testing.T) {
	s := &Shipper{NoBooking: true, Bookings: store.NewBookings()}
	if _, err := s.BookRoad(context.Background(), "serra-transportes.example", &intents.RoadBookingRequestBody{}); !errors.Is(err, ErrNoShipperRole) {
		t.Errorf("receiver booking: %v", err)
	}
	_ = s.Bookings.Add(store.Booking{Ref: "OB-BKG-1", Operator: "ocean-blue.example"})
	for _, ref := range []string{"OB-BKG-1", "SR-BKG-0913"} {
		if _, err := s.AmendRoad(context.Background(), "ocean-blue.example", ref, &intents.RoadBookingAmendmentBody{}); !errors.Is(err, ErrNotOurBooking) {
			t.Errorf("%s: %v", ref, err)
		}
	}
}

// TestRoadRefused: a carrier's signed refusal comes back as an error, and
// nothing is recorded or changed.
func TestRoadRefused(t *testing.T) {
	ctx := context.Background()
	s := shipperWith(t, func(envelope.Header) (string, any) {
		return intents.Error, &intents.ErrorBody{Code: intents.CodeForbidden, Detail: "No."}
	})
	if _, err := s.BookRoad(ctx, "ocean-blue.example", leg()); err == nil || s.Bookings.Len() != 0 {
		t.Errorf("book: %v", err)
	}
	_ = s.Bookings.Add(store.Booking{Ref: "OB-BKG-R-0913", Operator: "ocean-blue.example", Road: &store.RoadBooking{Request: *leg(), Confirmation: planned}})
	if _, err := s.AmendRoad(ctx, "ocean-blue.example", "OB-BKG-R-0913", &intents.RoadBookingAmendmentBody{Pickup: &intents.Window{Latest: "2026-10-24T20:00:00Z"}}); err == nil {
		t.Error("amend: accepted")
	}
	if b, _ := s.Bookings.Get("ocean-blue.example", "OB-BKG-R-0913"); b.Road.Request.Pickup.Latest != "" {
		t.Error("a refused amendment changed the leg")
	}
	if _, err := s.Status(ctx, "ocean-blue.example", "OB-BKG-R-0913"); err == nil {
		t.Error("an ocean status request for a truck leg")
	}
}

// TestAmendWindow: the times an amendment gives replace the stop's; those
// it leaves out stay.
func TestAmendWindow(t *testing.T) {
	s := intents.Stop{Earliest: "a", Latest: "b"}
	amendWindow(&s, nil)
	amendWindow(&s, &intents.Window{Latest: "c"})
	if s.Earliest != "a" || s.Latest != "c" {
		t.Errorf("%+v", s)
	}
	amendWindow(&s, &intents.Window{Earliest: "d"})
	if s.Earliest != "d" || s.Latest != "c" {
		t.Errorf("%+v", s)
	}
}

// TestShipperViewRoad: the facts the summarizer gives a model about a
// truck leg are its places, its planned delivery and its cargo.
func TestShipperViewRoad(t *testing.T) {
	v := shipperView(store.Booking{Ref: "OB-BKG-R-0913", Operator: "ocean-blue.example", Road: &store.RoadBooking{Request: *leg(), Confirmation: planned}})
	if v.Origin.UNLocode != "BRSSZ" || v.Destination.UNLocode != "BRSAO" || v.PlannedArrival != "2026-10-25T20:00:00Z" || v.Cargo != "Shoes" || v.Vessel != "" {
		t.Errorf("%+v", v)
	}
}

// TestAmendRoadRace: an amendment recorded while another was out keeps
// the record; the later answer isn't recorded over it.
func TestAmendRoadRace(t *testing.T) {
	moved := planned
	moved.PlannedPickup, moved.PlannedDelivery = "2026-10-27T14:00:00Z", "2026-10-28T20:00:00Z"
	var s *Shipper
	s = shipperWith(t, func(envelope.Header) (string, any) {
		// Another amendment is recorded while this one is out.
		_, _ = s.Bookings.Update("ocean-blue.example", "OB-BKG-R-0913", func(b *store.Booking) error {
			b.Road.Request.Delivery.Latest = "2026-10-30T12:00:00Z"
			return nil
		})
		return intents.RoadBookingConfirmation, &moved
	})
	_ = s.Bookings.Add(store.Booking{Ref: "OB-BKG-R-0913", Shipper: "cymbal-shoes.example", Operator: "ocean-blue.example",
		Road: &store.RoadBooking{Request: *leg(), Confirmation: planned}})
	_, err := s.AmendRoad(context.Background(), "ocean-blue.example", "OB-BKG-R-0913", &intents.RoadBookingAmendmentBody{
		Pickup: &intents.Window{Earliest: "2026-10-27T14:00:00Z"}, Delivery: &intents.Window{Latest: "2026-10-29T12:00:00Z"}})
	if !errors.Is(err, ErrLegChanged) {
		t.Errorf("%v, want ErrLegChanged", err)
	}
	if b, _ := s.Bookings.Get("ocean-blue.example", "OB-BKG-R-0913"); b.Road.Request.Delivery.Latest != "2026-10-30T12:00:00Z" || b.Road.Confirmation.PlannedPickup != planned.PlannedPickup {
		t.Errorf("record %+v", b.Road)
	}
}
