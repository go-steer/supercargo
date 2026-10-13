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

package forwarder

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/config"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/intents"
)

// TestDoorFirstLeg: a lane starting with a truck from the shipper's door
// picks up at the door's address; a truck that can't be reached, or a
// ship that's refused, stops the booking.
func TestDoorFirstLeg(t *testing.T) {
	doorToPort := config.Lane{From: "NLTLB", To: "BRSSZ", Legs: []config.LaneLeg{
		{Mode: "road", Carrier: "trucks.example", Name: "Trucks", To: "NLRTM", Facility: "Euromax terminal"},
		{Mode: "ocean", Carrier: "ocean-blue.example", Name: "Ocean Blue", To: "BRSSZ"},
	}}
	o := anOrder()
	o.PlaceOfReceipt = intents.Place{Type: intents.PlaceDoor, UNLocationCode: "NLTLB",
		Address: &intents.Address{Street: "Ringbaan Oost 100", Locality: "Tilburg", Country: "NL"}}
	legs := &carriers{}
	orch, _ := orchestrated(t, legs, doorToPort)
	if _, err := orch.Orders.Handle(context.Background(), orderFrom(cymbal, o)); err != nil {
		t.Fatal(err)
	}
	if _, err := orch.Book(context.Background(), "MF-0007"); err != nil {
		t.Fatal(err)
	}
	if r := legs.got; r.Pickup.Facility != "Ringbaan Oost 100, Tilburg" || r.Pickup.Address == nil || r.Delivery.Facility != "Euromax terminal" ||
		r.Consignee != nil || r.EquipmentReferences != nil || r.Pickup.Earliest != "2026-10-05T09:00:00Z" {
		t.Errorf("first truck %+v", r)
	}
	if len(legs.ocean) != 1 || legs.ocean[0] != "MF-0007-L2" {
		t.Errorf("ships booked %v", legs.ocean)
	}

	orch, _ = orchestrated(t, nil, doorToPort)
	orch.Legs = &failingRoad{}
	if _, err := orch.Orders.Handle(context.Background(), orderFrom(cymbal, o)); err != nil {
		t.Fatal(err)
	}
	if _, err := orch.Book(context.Background(), "MF-0007"); err == nil {
		t.Error("an unreachable truck")
	}
	orch, _ = orchestrated(t, nil, portToDoor)
	orch.Legs = refusedShip{&carriers{}}
	if _, err := orch.Orders.Handle(context.Background(), orderFrom(cymbal, doorOrder())); err != nil {
		t.Fatal(err)
	}
	if _, err := orch.Book(context.Background(), "MF-0007"); !errors.Is(err, ErrLegRefused) {
		t.Errorf("a refused ship: %v", err)
	}
}

type failingRoad struct{ carriers }

func (*failingRoad) BookRoad(context.Context, string, *intents.RoadBookingRequestBody) (*intents.RoadBookingConfirmationBody, error) {
	return nil, errors.New("connection refused")
}

type refusedShip struct{ *carriers }

func (refusedShip) Book(context.Context, string, *intents.BookingRequestBody) (*intents.BookingConfirmationBody, error) {
	return &intents.BookingConfirmationBody{BookingRef: "OB-BKG-1", Status: intents.StatusRejected}, nil
}

// TestAfterLeg: a ship's cargo is ready a handover after it arrives; a
// truck's on delivery; a leg without an arrival leaves the time as it was.
func TestAfterLeg(t *testing.T) {
	ready := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	if got := afterLeg(store.LegRef{Mode: intents.ModeOcean, Arrival: "2026-10-23T14:00:00Z"}, ready); !got.Equal(time.Date(2026, 10, 24, 20, 0, 0, 0, time.UTC)) {
		t.Errorf("ship: %v", got)
	}
	if got := afterLeg(store.LegRef{Mode: intents.ModeRoad, Arrival: "2026-10-06T15:00:00Z"}, ready); !got.Equal(time.Date(2026, 10, 6, 15, 0, 0, 0, time.UTC)) {
		t.Errorf("truck: %v", got)
	}
	if got := afterLeg(store.LegRef{}, ready); !got.Equal(ready) {
		t.Errorf("no arrival: %v", got)
	}
}
