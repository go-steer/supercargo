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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/config"
	"github.com/go-steer/supercargo/pkg/intents"
)

// carriers is a LegBooker: Ocean Blue sails NLRTM → BRSSZ, departing the
// day after the earliest asked at 18:00 for 380 hours; Serra trucks for
// 30 hours from the pickup window's start. refuseRoad makes Serra say no.
type carriers struct {
	ocean, road []string // the shipper references booked
	refuseRoad  bool
	oceanErr    error
	got         *intents.RoadBookingRequestBody
}

func (c *carriers) Book(_ context.Context, operator string, req *intents.BookingRequestBody) (*intents.BookingConfirmationBody, error) {
	if c.oceanErr != nil {
		return nil, c.oceanErr
	}
	c.ocean = append(c.ocean, req.ShipperRef)
	day, _ := time.Parse(intents.DateLayout, req.EarliestDeparture)
	dep := day.AddDate(0, 0, 1).Add(18 * time.Hour)
	return &intents.BookingConfirmationBody{BookingRef: "OB-BKG-1042", Status: intents.StatusConfirmed,
		PlannedDeparture: dep.Format(time.RFC3339), PlannedArrival: dep.Add(380 * time.Hour).Format(time.RFC3339),
		EBL: &intents.EBL{VesselName: "Ocean Blue Aurora", VoyageNumber: "42W", Containers: []intents.ShippingContainer{{ContainerNumber: "OCBU0000004"}}}}, nil
}

func (c *carriers) BookRoad(_ context.Context, carrier string, req *intents.RoadBookingRequestBody) (*intents.RoadBookingConfirmationBody, error) {
	c.got = req
	if c.refuseRoad {
		return &intents.RoadBookingConfirmationBody{BookingRef: "SR-BKG-0913", Status: intents.StatusRejected}, nil
	}
	c.road = append(c.road, req.ShipperRef)
	pickup, _ := time.Parse(time.RFC3339, req.Pickup.Earliest)
	return &intents.RoadBookingConfirmationBody{BookingRef: "SR-BKG-0913", Status: intents.StatusConfirmed,
		PlannedPickup: pickup.Format(time.RFC3339), PlannedDelivery: pickup.Add(30 * time.Hour).Format(time.RFC3339)}, nil
}

var lane = config.Lane{From: "NLRTM", To: "BRSSZ", Legs: []config.LaneLeg{
	{Mode: "ocean", Carrier: "ocean-blue.example", Name: "Ocean Blue", To: "BRSSZ", Facility: "Tecon"},
}}

// portToDoor is the lane the tests order on: by sea to Santos, then by
// truck to the door at Cajamar.
var portToDoor = config.Lane{From: "NLRTM", To: "BRSAO", Legs: []config.LaneLeg{
	{Mode: "ocean", Carrier: "ocean-blue.example", Name: "Ocean Blue", To: "BRSSZ", Facility: "Santos Brasil Tecon terminal"},
	{Mode: "road", Carrier: "serra-transportes.example", Name: "Serra Transportes", To: "BRSAO"},
}}

func doorOrder() *intents.ForwardingRequestBody {
	o := anOrder()
	o.PlaceOfReceipt.Earliest = "2026-10-06T06:00:00Z"
	o.PlaceOfDelivery = intents.Place{Type: intents.PlaceDoor, UNLocationCode: "BRSAO", Latest: "2026-11-27T12:00:00Z",
		Address: &intents.Address{Street: "Rodovia Anhanguera, km 33", Locality: "Cajamar", Country: "BR"}}
	return o
}

func orchestrated(t *testing.T, legs *carriers, lanes ...config.Lane) (*Orchestrator, *sent) {
	t.Helper()
	o, out := desk(t)
	return &Orchestrator{Orders: o, Legs: legs, Lanes: lanes, Name: "Meridian Forwarding", Endpoint: "https://meridian-forwarding.example/ulp",
		Now: func() time.Time { return time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC) }}, out
}

// TestPlanAndBook: the order is planned on its lane, then each leg is
// booked in turn: the truck collects the ship's container after its
// arrival and the handover, at the order's door by the order's date; and
// the order is answered with the journey.
func TestPlanAndBook(t *testing.T) {
	legs := &carriers{}
	orch, out := orchestrated(t, legs, lane, portToDoor)
	if _, err := orch.Orders.Handle(context.Background(), orderFrom(cymbal, doorOrder())); err != nil {
		t.Fatal(err)
	}
	steps, err := orch.Plan("MF-0007")
	if err != nil || len(steps) != 2 || steps[0].FromFacility != "Euromax" || steps[1].FromFacility != "Santos Brasil Tecon terminal" ||
		steps[1].ToFacility != "Rodovia Anhanguera, km 33, Cajamar" || steps[0].Booked != "" {
		t.Fatalf("plan %+v, %v", steps, err)
	}
	answer, err := orch.Book(context.Background(), "MF-0007")
	if err != nil {
		t.Fatal(err)
	}
	// The ship departs the 7th (the day after the cargo is ready), arrives
	// 380h later; the truck collects 30h after that.
	if len(answer.Legs) != 2 || answer.Legs[0].PlannedDeparture != "2026-10-07T18:00:00Z" || answer.Legs[0].PlannedArrival != "2026-10-23T14:00:00Z" ||
		answer.Legs[1].PlannedDeparture != "2026-10-24T20:00:00Z" || answer.PlannedDelivery != "2026-10-26T02:00:00Z" ||
		answer.Legs[0].Vessel != "Ocean Blue Aurora 42W" || answer.Legs[1].Carrier != "Serra Transportes" {
		t.Fatalf("answer %+v", answer)
	}
	if r := legs.got; r.ShipperRef != "MF-0007-L2" || r.Shipper.Domain != meridian || r.EquipmentReferences[0] != "OCBU0000004" ||
		r.Delivery.Latest != "2026-11-27T12:00:00Z" || r.Delivery.Address.Locality != "Cajamar" || r.Consignee == nil ||
		r.Subscription.Endpoint != "https://meridian-forwarding.example/ulp" {
		t.Errorf("truck request %+v", r)
	}
	if len(out.bodies) != 1 || out.bodies[0].Status != intents.StatusConfirmed {
		t.Errorf("answers sent: %+v", out.bodies)
	}
	b, _ := orch.Orders.Bookings.Get(meridian, "MF-0007")
	if len(b.Order.Legs) != 2 || b.Order.Legs[0].Ref != "OB-BKG-1042" || b.Order.Legs[1].Containers[0] != "OCBU0000004" {
		t.Errorf("legs recorded %+v", b.Order.Legs)
	}
	if steps, _ := orch.Plan("MF-0007"); steps[1].Booked != "SR-BKG-0913" {
		t.Errorf("plan after booking %+v", steps)
	}
}

// TestBookResumes: a carrier refusing its leg stops the booking with the
// legs before it recorded; booking again books only the rest.
func TestBookResumes(t *testing.T) {
	legs := &carriers{refuseRoad: true}
	orch, out := orchestrated(t, legs, portToDoor)
	if _, err := orch.Orders.Handle(context.Background(), orderFrom(cymbal, doorOrder())); err != nil {
		t.Fatal(err)
	}
	if _, err := orch.Book(context.Background(), "MF-0007"); !errors.Is(err, ErrLegRefused) {
		t.Fatalf("a refused truck: %v", err)
	}
	b, _ := orch.Orders.Bookings.Get(meridian, "MF-0007")
	if len(b.Order.Legs) != 1 || len(out.bodies) != 0 || b.Order.Confirmation.Status != intents.StatusPending {
		t.Fatalf("after the refusal: %d legs, %d answers", len(b.Order.Legs), len(out.bodies))
	}
	legs.refuseRoad = false
	if _, err := orch.Book(context.Background(), "MF-0007"); err != nil {
		t.Fatal(err)
	}
	if len(legs.ocean) != 1 || len(legs.road) != 1 || len(out.bodies) != 1 {
		t.Errorf("booked %d ships, %d trucks; %d answers", len(legs.ocean), len(legs.road), len(out.bodies))
	}
}

// TestBookRefused: an unknown order, one on no lane, one with an EDI leg
// and one whose ship can't be booked aren't answered, and nothing is
// booked for the first three.
func TestBookRefused(t *testing.T) {
	edi := portToDoor
	edi.Legs = []config.LaneLeg{portToDoor.Legs[0], {Mode: "road", Carrier: "polder-haulage.example", Name: "Polder", To: "BRSAO", EDI: true}}
	legs := &carriers{}
	orch, out := orchestrated(t, legs, edi)
	if _, err := orch.Book(context.Background(), "MF-9999"); !errors.Is(err, ErrUnknownOrder) {
		t.Errorf("unknown: %v", err)
	}
	if _, err := orch.Orders.Handle(context.Background(), orderFrom(cymbal, doorOrder())); err != nil {
		t.Fatal(err)
	}
	if _, err := orch.Book(context.Background(), "MF-0007"); !errors.Is(err, ErrEDILeg) || len(legs.ocean) != 0 {
		t.Errorf("an EDI leg: %v, %d ships booked", err, len(legs.ocean))
	}
	orch.Lanes = nil
	if _, err := orch.Book(context.Background(), "MF-0007"); !errors.Is(err, ErrNoLane) {
		t.Errorf("no lane: %v", err)
	}
	orch.Lanes = []config.Lane{portToDoor}
	legs.oceanErr = errors.New("connection refused")
	if _, err := orch.Book(context.Background(), "MF-0007"); err == nil || len(out.bodies) != 0 {
		t.Errorf("the ship unreachable: %v", err)
	}
	// Answered by hand meanwhile: not booked.
	if err := orch.Orders.Answer(context.Background(), "MF-0007", &intents.ForwardingConfirmationBody{Status: intents.StatusRejected}); err != nil {
		t.Fatal(err)
	}
	if _, err := orch.Book(context.Background(), "MF-0007"); !errors.Is(err, ErrAnswered) {
		t.Errorf("answered: %v", err)
	}
}

// TestControlPlanAndBook: the plan and the booking through the control
// API, with the outcome's status code.
func TestControlPlanAndBook(t *testing.T) {
	legs := &carriers{refuseRoad: true}
	orch, _ := orchestrated(t, legs, portToDoor)
	if _, err := orch.Orders.Handle(context.Background(), orderFrom(cymbal, doorOrder())); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	RegisterControl(mux, orch.Orders, orch)
	do := func(method, path string) int {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		return rec.Code
	}
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/orders/MF-0007/plan", http.StatusOK},
		{http.MethodGet, "/orders/MF-9999/plan", http.StatusNotFound},
		{http.MethodPost, "/orders/MF-9999/book", http.StatusNotFound},
		{http.MethodPost, "/orders/MF-0007/book", http.StatusBadGateway}, // the truck refused
	} {
		if code := do(tc.method, tc.path); code != tc.want {
			t.Errorf("%s %s: HTTP %d, want %d", tc.method, tc.path, code, tc.want)
		}
	}
	legs.refuseRoad = false
	if code := do(http.MethodPost, "/orders/MF-0007/book"); code != http.StatusOK {
		t.Errorf("book: HTTP %d", code)
	}
	if code := do(http.MethodPost, "/orders/MF-0007/book"); code != http.StatusConflict {
		t.Errorf("book again: HTTP %d", code)
	}
	orch.Lanes = nil
	if code := do(http.MethodGet, "/orders/MF-0007/plan"); code != http.StatusUnprocessableEntity {
		t.Errorf("no lane: HTTP %d", code)
	}
}
