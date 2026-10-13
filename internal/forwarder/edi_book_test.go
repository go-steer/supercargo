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
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/config"
	"github.com/go-steer/supercargo/pkg/intents"
)

// TestBookEDILeg: a lane starting with a carrier without a node books its
// leg over EDI (the instruction acknowledged, the leg marked unsigned with
// the e-CMR, planned on the partner's transit), then the ship after it.
func TestBookEDILeg(t *testing.T) {
	r := newEDIRig(t)
	r.run(t)
	tilburgToSantos := config.Lane{From: "NLTLB", To: "BRSSZ", Legs: []config.LaneLeg{
		{Mode: "road", Carrier: polder, Name: "Polder Haulage", To: "NLRTM", Facility: "Euromax terminal", EDI: true},
		{Mode: "ocean", Carrier: "ocean-blue.example", Name: "Ocean Blue", To: "BRSSZ"},
	}}
	legs := &carriers{}
	orch, out := orchestrated(t, legs, tilburgToSantos)
	orch.EDI = r.gw
	r.polder.Partner = meridian
	o := anOrder()
	o.PlaceOfReceipt = intents.Place{Type: intents.PlaceDoor, UNLocationCode: "NLTLB",
		Address: &intents.Address{Street: "Ringbaan Oost 100", Locality: "Tilburg", PostalCode: "5013 CD", Country: "NL"}}
	if _, err := orch.Orders.Handle(context.Background(), orderFrom(cymbal, o)); err != nil {
		t.Fatal(err)
	}
	answer, err := orch.Book(context.Background(), "MF-0007")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := orch.Orders.Bookings.Get(meridian, "MF-0007")
	l := b.Order.Legs[0]
	if !l.Unsigned || l.Ref != "MF-0007-L1" || l.CarrierRef == "" || l.Documents[0] != "ECMR-NL-2026-004471" ||
		l.Departure != "2026-10-05T09:00:00Z" || l.Arrival != "2026-10-05T12:00:00Z" {
		t.Errorf("the EDI leg %+v", l)
	}
	if len(answer.Legs) != 2 || answer.Legs[0].Carrier != "Polder Haulage" || len(legs.ocean) != 1 || len(out.bodies) != 1 {
		t.Errorf("answer %+v", answer)
	}
	// The ship departs the day after the truck delivers to the terminal.
	if answer.Legs[1].PlannedDeparture != "2026-10-06T18:00:00Z" {
		t.Errorf("ship departs %s", answer.Legs[1].PlannedDeparture)
	}
}

// TestGatewayRun: Run takes replies until its context ends.
func TestGatewayRun(t *testing.T) {
	r := newEDIRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	r.gw.Run(ctx, 10*time.Millisecond)
}
