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

package store

import (
	"errors"
	"slices"
	"testing"

	"github.com/go-steer/supercargo/pkg/intents"
)

func roadBooking() Booking {
	return Booking{Ref: "SR-BKG-0913", Shipper: "meridian-forwarding.example", Operator: "serra-transportes.example",
		Road: &RoadBooking{
			Request: intents.RoadBookingRequestBody{ShipperRef: "MF-0007-L3",
				Shipper:             intents.Party{Name: "Meridian", Address: &intents.Address{Street: "Wilhelminakade 1", Locality: "Rotterdam", Country: "NL"}},
				Pickup:              intents.Stop{Facility: "Tecon", UNLocationCode: "BRSSZ"},
				Delivery:            intents.Stop{Facility: "DC", UNLocationCode: "BRSAO"},
				EquipmentReferences: []string{"OCBU0000004"},
				Subscription:        &intents.Subscription{Endpoint: "https://meridian-forwarding.example/ulp", Intents: []string{intents.TnTEvent}}},
			Confirmation: intents.RoadBookingConfirmationBody{BookingRef: "SR-BKG-0913", Status: intents.StatusConfirmed,
				PlannedPickup: "2026-10-24T14:00:00Z", PlannedDelivery: "2026-10-25T20:00:00Z", SubscriptionID: "SUB-0913"},
		}}
}

// TestBookingViews: the views read an ocean booking's fields, or a truck
// leg's.
func TestBookingViews(t *testing.T) {
	o := booking("ocean-blue.example", "OB-BKG-1")
	o.Request.ShipperRef = "P1"
	o.Request.Subscription = &intents.Subscription{Endpoint: "https://cymbal-shoes.example/ulp"}
	o.Confirmation.SubscriptionID = "SUB-1"
	o.Confirmation.EBL = &intents.EBL{Containers: []intents.ShippingContainer{{ContainerNumber: "OCBU0000004"}}}
	if from, to := o.Places(); from != o.Request.PortOfLoading || to != o.Request.PortOfDischarge || o.Subscription() != o.Request.Subscription ||
		o.SubscriptionID() != "SUB-1" || !slices.Equal(o.EquipmentRefs(), []string{"OCBU0000004"}) || o.ShipperRef() != "P1" {
		t.Errorf("ocean views")
	}
	r := roadBooking()
	if from, to := r.Places(); from != "BRSSZ" || to != "BRSAO" || r.Subscription() != r.Road.Request.Subscription ||
		r.SubscriptionID() != "SUB-0913" || !slices.Equal(r.EquipmentRefs(), []string{"OCBU0000004"}) || r.ShipperRef() != "MF-0007-L3" {
		t.Errorf("road views")
	}
	// EquipmentRefs is a copy.
	r.EquipmentRefs()[0] = "X"
	if r.Road.Request.EquipmentReferences[0] != "OCBU0000004" {
		t.Error("EquipmentRefs shares the request's slice")
	}
}

// TestRoadBookingCopied: the store copies a truck leg deeply, in and out.
func TestRoadBookingCopied(t *testing.T) {
	s := NewBookings()
	b := roadBooking()
	if err := s.Add(b); err != nil {
		t.Fatal(err)
	}
	b.Road.Request.Shipper.Address.Street = "changed"
	b.Road.Request.Subscription.Intents[0] = "changed"
	got, _ := s.Get(b.Operator, b.Ref)
	if got.Road.Request.Shipper.Address.Street != "Wilhelminakade 1" || got.Road.Request.Subscription.Intents[0] != intents.TnTEvent {
		t.Errorf("the caller changed the stored leg: %+v", got.Road.Request)
	}
	got.Road.Confirmation.PlannedPickup = "changed"
	if again, _ := s.Get(b.Operator, b.Ref); again.Road.Confirmation.PlannedPickup != "2026-10-24T14:00:00Z" {
		t.Error("a reader changed the stored leg")
	}
}

// TestUpdate: a change applies atomically to a copy; a failed change, or
// one that tries to move the booking, changes nothing it shouldn't.
func TestUpdate(t *testing.T) {
	s := NewBookings()
	b := roadBooking()
	_ = s.Add(b)
	if _, err := s.Update(b.Operator, "SR-BKG-9999", func(*Booking) error { return nil }); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
	boom := errors.New("boom")
	if _, err := s.Update(b.Operator, b.Ref, func(b *Booking) error {
		b.Road.Confirmation.PlannedPickup = "half-done"
		return boom
	}); !errors.Is(err, boom) {
		t.Errorf("failed change: %v", err)
	}
	if got, _ := s.Get(b.Operator, b.Ref); got.Road.Confirmation.PlannedPickup != "2026-10-24T14:00:00Z" {
		t.Error("a failed change applied")
	}
	got, err := s.Update(b.Operator, b.Ref, func(b *Booking) error {
		b.Road.Confirmation.PlannedPickup = "2026-10-27T14:00:00Z"
		b.Ref, b.Operator = "SR-BKG-0001", "other.example" // can't move it
		return nil
	})
	if err != nil || got.Ref != b.Ref || got.Operator != b.Operator || got.Road.Confirmation.PlannedPickup != "2026-10-27T14:00:00Z" {
		t.Fatalf("%+v, %v", got, err)
	}
	if stored, _ := s.Get(b.Operator, b.Ref); stored.Road.Confirmation.PlannedPickup != "2026-10-27T14:00:00Z" || s.Len() != 1 {
		t.Error("the change wasn't stored in place")
	}
	got.Road.Confirmation.PlannedPickup = "changed"
	if stored, _ := s.Get(b.Operator, b.Ref); stored.Road.Confirmation.PlannedPickup != "2026-10-27T14:00:00Z" {
		t.Error("Update returned the stored copy")
	}
}

// TestRoadSummaryAndJourney: a truck leg sums up and maps from its pickup
// and delivery.
func TestRoadSummaryAndJourney(t *testing.T) {
	b := roadBooking()
	s := Summarize(b, nil)
	if s.Origin.UNLocode != "BRSSZ" || s.Destination.UNLocode != "BRSAO" || s.Status != intents.StatusConfirmed ||
		s.PlannedDeparture != "2026-10-24T14:00:00Z" || s.ETA != "2026-10-25T20:00:00Z" || len(s.Containers) != 1 {
		t.Errorf("summary %+v", s)
	}
	j := BuildJourney(b, nil)
	if len(j.Legs) != 1 || j.Legs[0].Departure.At != "2026-10-24T14:00:00Z" || j.Legs[0].Arrival.At != "2026-10-25T20:00:00Z" {
		t.Errorf("journey %+v", j.Legs)
	}
}
