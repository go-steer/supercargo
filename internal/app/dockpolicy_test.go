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
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/activity"
	"github.com/go-steer/supercargo/internal/docks"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/delegation"
	"github.com/go-steer/supercargo/pkg/intents"
)

// docksSaid is what the node's dock policy has said so far.
func docksSaid(a *App) []activity.Dock {
	past, _, _, cancel := a.Activity.Subscribe(0, 1)
	defer cancel()
	var out []activity.Dock
	for _, e := range past {
		if e.Dock != nil {
			out = append(out, *e.Dock)
		}
	}
	return out
}

// TestDockPolicy: a DC with a standing dock policy books a slot for a
// truck it's granted, at the leg's planned delivery, and moves it when a
// verified alert from the carrier says the truck will be late; each
// appointment says whose policy did it.
func TestDockPolicy(t *testing.T) {
	rig := map[string]string{}
	for k, v := range delegationRig {
		rig[k] = v
	}
	rig[aurora] += `docks: { mock: "Agendamento (simulated)", site: Guarulhos DC, zone: America/Sao_Paulo, docks: [Dock 1, Dock 2], hours: [6, 10, 14, 18, 22], policy: { by: Receiving manager } }` + "\n"
	apps := startNodes(t, rig)
	vv, sr, dc := apps[valeVerde], apps[serraTr], apps[aurora]
	ctx := context.Background()
	conf, err := vv.Shipper.BookRoad(ctx, serraTr, &intents.RoadBookingRequestBody{
		ShipperRef: "VV-FTL-1", Shipper: intents.Party{Name: "Vale Verde", Domain: valeVerde},
		Pickup:       intents.Stop{Facility: "Fazenda Boa Safra", UNLocationCode: "BRSAO"},
		Delivery:     intents.Stop{Facility: "Mercado Aurora DC", UNLocationCode: "BRGRU"},
		Equipment:    []intents.Equipment{{ISOType: "45R1", Count: 1, GrossWeightKg: 26000}},
		Cargo:        intents.Cargo{Description: "Chilled beef", HSCode: "0201"},
		Subscription: &intents.Subscription{Endpoint: vv.Shipper.Endpoint, Intents: []string{intents.TnTEvent, intents.TnTAlert}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ref := conf.BookingRef
	if _, err := vv.Shipper.Grant(ctx, serraTr, ref, aurora, []string{delegation.ScopeRead, delegation.ScopeSubscribe}, 7*24*time.Hour, ""); err != nil {
		t.Fatal(err)
	}
	until(t, "the policy's booking", func() bool { return len(docksSaid(dc)) == 1 })
	booked := docksSaid(dc)[0]
	planned, _ := time.Parse(time.RFC3339, conf.PlannedDelivery)
	if booked.Action != "booked" || booked.Booking != ref || booked.Policy != "standing dock policy, set by Receiving manager" || booked.End.Before(planned.Add(time.Hour)) {
		t.Fatalf("booked %+v (planned %s)", booked, planned)
	}
	// Rain: the truck is 30 hours late.
	if _, err := sr.Simulator.Disrupt(ref, 30*time.Hour, "Heavy rain on BR-163 near Sinop."); err != nil {
		t.Fatal(err)
	}
	until(t, "the policy's move", func() bool { return len(docksSaid(dc)) == 2 })
	moved := docksSaid(dc)[1]
	if moved.Action != "moved" || !moved.FromStart.Equal(booked.Start) || moved.End.Before(planned.Add(31*time.Hour)) {
		t.Errorf("moved %+v", moved)
	}
	a, err := dc.Docks.Appointment(ctx, docks.TruckKey(serraTr, ref))
	if err != nil || !a.Start.Equal(moved.Start) || a.Policy != booked.Policy {
		t.Errorf("appointment %+v %v", a, err)
	}
	// The shipper isn't the DC: it has no dock policy and learns nothing of it.
	if len(docksSaid(vv)) != 0 {
		t.Errorf("the shipper's docks: %+v", docksSaid(vv))
	}
	// A renewed grant re-subscribes with the leg's original plan: the
	// policy keeps the slot the late truck needs, never moves it back.
	if _, err := vv.Shipper.Grant(ctx, serraTr, ref, aurora, []string{delegation.ScopeRead, delegation.ScopeSubscribe}, 10*24*time.Hour, ""); err != nil {
		t.Fatal(err)
	}
	until(t, "the renewal's reservation", func() bool { return len(docksSaid(dc)) == 3 })
	if kept := docksSaid(dc)[2]; kept.Action != "kept" || !kept.Start.Equal(moved.Start) {
		t.Errorf("renewed: %+v", kept)
	}
	// A booking the DC isn't following by delegation never reaches the policy.
	dc.keepDock(store.Booking{Ref: ref, Operator: serraTr}, moved.Start.Format(time.RFC3339), dockAlerted)
	if n := len(docksSaid(dc)); n != 3 {
		t.Errorf("an undelegated booking: %d entries", n)
	}
}
