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
	"github.com/go-steer/supercargo/pkg/intents"
)

// meanwhile is a LegBooker that does something while its ship is booked.
type meanwhile struct {
	*carriers
	during func()
}

func (m meanwhile) Book(ctx context.Context, op string, req *intents.BookingRequestBody) (*intents.BookingConfirmationBody, error) {
	m.during()
	return m.carriers.Book(ctx, op, req)
}

// TestAnswerWhileBooking: a person's answer while the legs are being
// booked is refused, and the booking goes on to answer the order itself:
// no carrier is booked for a rejected order, and both sides agree.
func TestAnswerWhileBooking(t *testing.T) {
	legs := &carriers{}
	orch, out := orchestrated(t, legs, portToDoor)
	var during error
	orch.Legs = meanwhile{legs, func() {
		during = orch.Orders.Answer(context.Background(), "MF-0007", &intents.ForwardingConfirmationBody{Status: intents.StatusRejected})
	}}
	if _, err := orch.Orders.Handle(context.Background(), orderFrom(cymbal, doorOrder())); err != nil {
		t.Fatal(err)
	}
	if _, err := orch.Book(context.Background(), "MF-0007"); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(during, ErrAnswered) {
		t.Errorf("an answer while booking: %v", during)
	}
	if len(out.bodies) != 1 || out.bodies[0].Status != intents.StatusConfirmed {
		t.Errorf("answers sent: %+v", out.bodies)
	}
}

// early is a LegBooker whose ship leaves before the cargo is ready.
type early struct{ *carriers }

func (e early) Book(ctx context.Context, op string, req *intents.BookingRequestBody) (*intents.BookingConfirmationBody, error) {
	c, err := e.carriers.Book(ctx, op, req)
	if err == nil {
		dep, _ := time.Parse(time.RFC3339, c.PlannedDeparture)
		c.PlannedDeparture = dep.Add(-48 * time.Hour).Format(time.RFC3339)
	}
	return c, err
}

// TestPlanUnfit: a ship planned to leave before the cargo is ready isn't
// used; a ship arriving too late for the order's date stops the booking
// before the truck; neither is recorded nor answered.
func TestPlanUnfit(t *testing.T) {
	legs := &carriers{}
	orch, out := orchestrated(t, legs, portToDoor)
	orch.Legs = early{legs}
	if _, err := orch.Orders.Handle(context.Background(), orderFrom(cymbal, doorOrder())); err != nil {
		t.Fatal(err)
	}
	if _, err := orch.Book(context.Background(), "MF-0007"); !errors.Is(err, ErrPlanUnfit) {
		t.Errorf("a ship leaving early: %v", err)
	}
	if b, _ := orch.Orders.Bookings.Get(meridian, "MF-0007"); len(b.Order.Legs) != 0 || len(out.bodies) != 0 {
		t.Errorf("recorded %d legs, sent %d answers", len(b.Order.Legs), len(out.bodies))
	}

	legs = &carriers{}
	orch, out = orchestrated(t, legs, portToDoor)
	soon := doorOrder()
	soon.PlaceOfDelivery.Latest = "2026-10-24T00:00:00Z" // the ship arrives the 23rd, ready for the truck the 24th at 20:00
	if _, err := orch.Orders.Handle(context.Background(), orderFrom(cymbal, soon)); err != nil {
		t.Fatal(err)
	}
	if _, err := orch.Book(context.Background(), "MF-0007"); !errors.Is(err, ErrPlanUnfit) || len(legs.road) != 0 || len(out.bodies) != 0 {
		t.Errorf("a date the ship can't make: %v, %d trucks booked", err, len(legs.road))
	}

	// An ocean last leg arriving after the order's date.
	legs = &carriers{}
	orch, _ = orchestrated(t, legs, lane)
	port := anOrder()
	port.PlaceOfDelivery.Latest = "2026-10-10T00:00:00Z"
	if _, err := orch.Orders.Handle(context.Background(), orderFrom(cymbal, port)); err != nil {
		t.Fatal(err)
	}
	if _, err := orch.Book(context.Background(), "MF-0007"); !errors.Is(err, ErrPlanUnfit) {
		t.Errorf("a ship arriving late: %v", err)
	}
}

// TestPlanChanged: legs booked on a lane edited since aren't resumed.
func TestPlanChanged(t *testing.T) {
	legs := &carriers{refuseRoad: true}
	orch, _ := orchestrated(t, legs, portToDoor)
	if _, err := orch.Orders.Handle(context.Background(), orderFrom(cymbal, doorOrder())); err != nil {
		t.Fatal(err)
	}
	if _, err := orch.Book(context.Background(), "MF-0007"); !errors.Is(err, ErrLegRefused) {
		t.Fatal(err)
	}
	edited := portToDoor
	edited.Legs = []config.LaneLeg{{Mode: "ocean", Carrier: "other-line.example", Name: "Other Line", To: "BRSSZ", Facility: "Tecon"}, portToDoor.Legs[1]}
	orch.Lanes = []config.Lane{edited}
	legs.refuseRoad = false
	if _, err := orch.Book(context.Background(), "MF-0007"); !errors.Is(err, ErrPlanChanged) || len(legs.road) != 0 {
		t.Errorf("an edited lane: %v", err)
	}
}

// TestDoorNeedsTruck: a door order isn't planned on a lane that reaches
// its door by sea.
func TestDoorNeedsTruck(t *testing.T) {
	seaToDoor := config.Lane{From: "NLRTM", To: "BRSAO", Legs: []config.LaneLeg{{Mode: "ocean", Carrier: "ocean-blue.example", Name: "Ocean Blue", To: "BRSAO"}}}
	orch, _ := orchestrated(t, &carriers{}, seaToDoor)
	if _, err := orch.Orders.Handle(context.Background(), orderFrom(cymbal, doorOrder())); err != nil {
		t.Fatal(err)
	}
	if _, err := orch.Plan("MF-0007"); !errors.Is(err, ErrNoLane) {
		t.Errorf("a door by sea: %v", err)
	}
	if got := clip("Ocean Blue Aurora", 5); got != "Ocean" {
		t.Errorf("clip: %q", got)
	}
}
