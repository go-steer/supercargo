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

package app

import (
	"context"
	"encoding/json"
	"maps"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/forwarder"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/intents"
)

// waitFor polls cond for up to 20 seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// records decodes the events and alerts a log holds for one booking.
func records(recs []store.Record) (events []intents.TnTEventBody, alerts []intents.TnTAlertBody) {
	for _, r := range recs {
		switch r.Intent {
		case intents.TnTEvent:
			var e intents.TnTEventBody
			if json.Unmarshal(r.Body, &e) == nil {
				events = append(events, e)
			}
		case intents.TnTAlert:
			var a intents.TnTAlertBody
			if json.Unmarshal(r.Body, &a) == nil {
				alerts = append(alerts, a)
			}
		}
	}
	return events, alerts
}

// TestCascade: the carriers' signed events reach Cymbal as Meridian's,
// each derived from its leg's; the ship's delay becomes Meridian's delay
// alert on the order, and a proposal to re-time the truck, which, once
// Meridian's person confirms it, Serra re-plans.
func TestCascade(t *testing.T) {
	rig := maps.Clone(journeyRig)
	rig["ocean-blue.example"] += "simulator: { hoursPerSecond: 20 }\n"
	apps := startNodes(t, rig)
	cymbal, meridian, ocean := apps["cymbal-shoes.example"], apps["meridian-forwarding.example"], apps["ocean-blue.example"]
	ctx := context.Background()
	pending, err := cymbal.Shipper.Order(ctx, "meridian-forwarding.example", portOrder())
	if err != nil {
		t.Fatal(err)
	}
	ref := pending.OrderRef
	if _, err := meridian.Orchestrator.Book(ctx, ref); err != nil {
		t.Fatal(err)
	}
	order, _ := meridian.Carrier.Bookings.Get("meridian-forwarding.example", ref)
	ship := order.Order.Legs[0]

	// The ship's gate-in reaches Cymbal as Meridian's event on the order.
	held := func() []store.Record { return cymbal.Customer.Events.List("meridian-forwarding.example", ref) }
	waitFor(t, "a relayed event", func() bool { events, _ := records(held()); return len(events) > 0 })
	events, _ := records(held())
	if e := events[0]; len(e.DerivedFrom) != 1 || !e.DerivedFrom[0].Signed || e.DerivedFrom[0].ID == "" || e.EventID[:3] != "MF-" {
		t.Errorf("relayed event %+v", e)
	}

	// The ship is 72 hours late.
	if _, err := ocean.Simulator.Disrupt(ship.Ref, 72*time.Hour, "Engine repair at Las Palmas."); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the order's delay alert", func() bool { _, alerts := records(held()); return len(alerts) > 0 })
	_, alerts := records(held())
	a := alerts[0]
	if a.Kind != intents.AlertDelay || len(a.DerivedFrom) != 1 || !a.DerivedFrom[0].Signed || len(a.CauseEventIDs) != 1 {
		t.Fatalf("the order's alert %+v", a)
	}
	prev, _ := time.Parse(time.RFC3339, a.PreviousETA)
	next, _ := time.Parse(time.RFC3339, a.NewETA)
	if next.Sub(prev) != 72*time.Hour || a.PreviousETA != order.Order.Legs[1].Arrival {
		t.Errorf("the order's delivery moves %v, from %s", next.Sub(prev), a.PreviousETA)
	}
	// Its cause is an event Cymbal holds: Meridian's re-issued estimate.
	found := false
	events, _ = records(held())
	for _, e := range events {
		found = found || e.EventID == a.CauseEventIDs[0]
	}
	if !found {
		t.Errorf("the alert cites %s, which Cymbal doesn't hold", a.CauseEventIDs[0])
	}

	// A proposal waits for Meridian's person; confirmed, Serra re-plans.
	var proposals []forwarder.Proposal
	waitFor(t, "a proposal", func() bool { proposals = meridian.Relay.Proposals(ref); return len(proposals) == 1 })
	p := proposals[0]
	if p.Leg != 2 || p.Carrier != "serra-transportes.example" || p.Status != forwarder.ProposalOpen {
		t.Fatalf("proposal %+v", p)
	}
	truck, _ := meridian.Customer.Bookings.Get("serra-transportes.example", p.LegRef)
	if truck.Road.Request.Pickup.Earliest == p.Amendment.Pickup.Earliest {
		t.Fatal("the truck was re-timed before anyone confirmed")
	}
	done, err := meridian.Relay.Confirm(ctx, p.ID)
	if err != nil || done.Status != forwarder.ProposalConfirmed {
		t.Fatalf("confirmed %+v, %v", done, err)
	}
	truck, _ = meridian.Customer.Bookings.Get("serra-transportes.example", p.LegRef)
	if truck.Road.Request.Pickup.Earliest != p.Amendment.Pickup.Earliest || truck.Road.Confirmation.PlannedPickup != p.Amendment.Pickup.Earliest {
		t.Errorf("the truck's leg %+v", truck.Road)
	}
	order, _ = meridian.Carrier.Bookings.Get("meridian-forwarding.example", ref)
	if order.Order.Legs[1].Departure != p.Amendment.Pickup.Earliest || order.Order.Legs[1].Arrival != a.NewETA {
		t.Errorf("the order's record of leg 2 %+v, want arriving %s", order.Order.Legs[1], a.NewETA)
	}
}
