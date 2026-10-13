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
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/edi"
	"github.com/go-steer/supercargo/internal/mockcarrier"
	"github.com/go-steer/supercargo/pkg/intents"
)

// TestTilburgToCajamar: the whole door-to-door journey. Meridian books
// Polder's truck from Cymbal's warehouse over EDI (Polder has no node),
// Ocean Blue's ship and Serra's truck over ULP, and answers Cymbal's
// order with the three legs; Polder's EDI statuses reach Cymbal as
// Meridian's events, marked derived from an unsigned source.
func TestTilburgToCajamar(t *testing.T) {
	outbox, inbox := t.TempDir(), t.TempDir()
	rig := maps.Clone(journeyRig)
	rig["meridian-forwarding.example"] = `name: Meridian Forwarding
roles: [operator, shipper]
forwarder:
  refPrefix: MF
  edi:
    - { carrier: polder-haulage.example, id: polder-haulage.example, outbox: "` + outbox + `", inbox: "` + inbox + `", transit: 3h }
  lanes:
    - from: NLTLB
      to: BRSAO
      legs:
        - { mode: road, carrier: polder-haulage.example, name: Polder Haulage, to: NLRTM, facility: Euromax terminal, edi: true }
        - { mode: ocean, carrier: ocean-blue.example, name: Ocean Blue, to: BRSSZ, facility: Santos Brasil Tecon terminal }
        - { mode: road, carrier: serra-transportes.example, name: Serra Transportes, to: BRSAO }
`
	apps := startNodes(t, rig)
	cymbal, meridian := apps["cymbal-shoes.example"], apps["meridian-forwarding.example"]

	// Polder, on the other side of Meridian's file drops.
	in, err := edi.NewDir(outbox)
	if err != nil {
		t.Fatal(err)
	}
	out, err := edi.NewDir(inbox)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = in.Close(); _ = out.Close() })
	polder := &mockcarrier.EDIFACT{ID: "polder-haulage.example", Partner: "meridian-forwarding.example", In: in, Out: out,
		ECMRPrefix: "ECMR-NL-2026", HoursPerSecond: 3600, Now: time.Now, Log: quiet()}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- polder.Run(ctx, 20*time.Millisecond) }()
	t.Cleanup(func() { cancel(); <-done })

	o := order() // Tilburg's door to Cajamar's
	o.PlaceOfDelivery.Latest = time.Now().Add(60 * 24 * time.Hour).UTC().Truncate(time.Hour).Format("2006-01-02T15:04:05Z")
	pending, err := cymbal.Shipper.Order(context.Background(), "meridian-forwarding.example", o)
	if err != nil {
		t.Fatal(err)
	}
	answer, err := meridian.Orchestrator.Book(context.Background(), pending.OrderRef)
	if err != nil {
		t.Fatal(err)
	}
	if len(answer.Legs) != 3 || answer.Legs[0].Carrier != "Polder Haulage" || answer.Legs[1].Vessel == "" || answer.Legs[2].Carrier != "Serra Transportes" {
		t.Fatalf("answer %+v", answer)
	}
	order, _ := meridian.Carrier.Bookings.Get("meridian-forwarding.example", pending.OrderRef)
	if l := order.Order.Legs[0]; !l.Unsigned || len(l.Documents) != 1 || !strings.HasPrefix(l.Documents[0], "ECMR-NL-2026-") {
		t.Errorf("Polder's leg %+v", l)
	}

	// Polder's statuses arrive as Meridian's events on the order, unsigned.
	held := func() []intents.TnTEventBody {
		events, _ := records(cymbal.Customer.Events.List("meridian-forwarding.example", pending.OrderRef))
		var unsigned []intents.TnTEventBody
		for _, e := range events {
			if len(e.DerivedFrom) == 1 && !e.DerivedFrom[0].Signed {
				unsigned = append(unsigned, e)
			}
		}
		return unsigned
	}
	waitFor(t, "Polder's statuses at Cymbal", func() bool { return len(held()) >= 4 })
	e := held()[0]
	if e.DerivedFrom[0].Source != "EDIFACT IFTSTA from polder-haulage.example" || !strings.Contains(e.Description, "reported over EDI") ||
		e.UNLocationCode != "NLTLB" {
		t.Errorf("Polder's first status at Cymbal %+v", e)
	}
}
