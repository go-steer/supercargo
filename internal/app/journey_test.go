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
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/config"
	"github.com/go-steer/supercargo/internal/forwarder"
	"github.com/go-steer/supercargo/pkg/intents"
)

// startNodes starts one node per domain, each from its YAML (without
// domain, listen, keys, tls and peers, which are filled in), all trusting
// one local CA and knowing each other's addresses. Every node pins every
// other as a partner.
func startNodes(t *testing.T, nodes map[string]string) map[string]*App {
	t.Helper()
	var domains []string
	for d := range nodes {
		domains = append(domains, d)
	}
	cert, key, ca := tlsFiles(t, domains...)
	addrs := map[string]string{}
	var peers []string
	for _, d := range domains {
		addrs[d] = freePort(t)
		peers = append(peers, d+`: "`+addrs[d]+`"`)
	}
	tlsYAML := `tls: { mode: local-ca, certFile: "` + cert + `", keyFile: "` + key + `", caFile: "` + ca + `" }
peers: { ` + strings.Join(peers, ", ") + ` }
`
	apps := map[string]*App{}
	for _, d := range domains {
		var partners []string
		for _, p := range domains {
			if p != d {
				partners = append(partners, p)
			}
		}
		c, err := config.Parse([]byte("domain: " + d + "\nlisten: \"" + addrs[d] + "\"\nkeys: [{ kid: k-1, privateKeyFile: \"" + keyFile(t) + "\" }]\n" +
			"partners: [" + strings.Join(partners, ", ") + "]\n" + tlsYAML + nodes[d]))
		if err != nil {
			t.Fatalf("%s: %v", d, err)
		}
		a, err := New(c, quiet())
		if err != nil {
			t.Fatal(err)
		}
		apps[d] = a
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- a.Serve(ctx) }()
		t.Cleanup(func() { cancel(); <-done })
		waitListening(t, c.Listen)
	}
	return apps
}

// The door-to-door rig, port to door: Meridian runs the lane Rotterdam →
// Cajamar on Ocean Blue to Santos and Serra's trucks on to the door.
var journeyRig = map[string]string{
	"cymbal-shoes.example": "name: Cymbal Shoes\nrole: shipper\n",
	"meridian-forwarding.example": `name: Meridian Forwarding
roles: [operator, shipper]
forwarder:
  refPrefix: MF
  lanes:
    - from: NLRTM
      to: BRSAO
      legs:
        - { mode: ocean, carrier: ocean-blue.example, name: Ocean Blue, to: BRSSZ, facility: Santos Brasil Tecon terminal }
        - { mode: road, carrier: serra-transportes.example, name: Serra Transportes, to: BRSAO }
`,
	"ocean-blue.example": "name: Ocean Blue\nrole: operator\noperator: { refPrefix: OB-BKG, containerPrefix: OCBU, vessel: Ocean Blue Aurora, voyage: 42W, departureHour: 18, transit: 380h }\n",
	"serra-transportes.example": `name: Serra Transportes
role: operator
operator: { refPrefix: SR-BKG, road: { transit: 30h, cte: { cnpj: "12345678000190", state: 35 } } }
`,
}

func portOrder() *intents.ForwardingRequestBody {
	o := order()
	o.PlaceOfReceipt = intents.Place{Type: intents.PlacePort, Facility: "Euromax terminal", UNLocationCode: "NLRTM"}
	o.PlaceOfDelivery.Latest = time.Now().Add(60 * 24 * time.Hour).UTC().Truncate(time.Hour).Format("2006-01-02T15:04:05Z")
	return o
}

// TestDoorToDoor: Meridian takes Cymbal's order as pending, plans it on
// its lane, and, once its person confirms, books the ship and then the
// truck (collecting the ship's container after its arrival), and answers
// the order with the journey, which Cymbal holds.
func TestDoorToDoor(t *testing.T) {
	apps := startNodes(t, journeyRig)
	cymbal, meridian := apps["cymbal-shoes.example"], apps["meridian-forwarding.example"]
	ctx := context.Background()
	ordered := portOrder()
	pending, err := cymbal.Shipper.Order(ctx, "meridian-forwarding.example", ordered)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := meridian.Orchestrator.Plan(pending.OrderRef)
	if err != nil || len(steps) != 2 || steps[1].FromFacility != "Santos Brasil Tecon terminal" || steps[1].ToFacility != "Cymbal Calçados DC, dock 2" {
		t.Fatalf("plan %+v, %v", steps, err)
	}
	answer, err := meridian.Orchestrator.Book(ctx, pending.OrderRef)
	if err != nil {
		t.Fatal(err)
	}
	if len(answer.Legs) != 2 || answer.Legs[0].Vessel != "Ocean Blue Aurora 42W" || answer.Legs[1].Carrier != "Serra Transportes" {
		t.Fatalf("answer %+v", answer)
	}
	mine, _ := cymbal.Customer.Bookings.Get("meridian-forwarding.example", pending.OrderRef)
	if c := mine.Order.Confirmation; c.Status != intents.StatusConfirmed || c.PlannedDelivery != answer.PlannedDelivery {
		t.Errorf("Cymbal holds %+v", c)
	}
	// Meridian holds each leg as its shipper, and the order's record of them.
	order, _ := meridian.Carrier.Bookings.Get("meridian-forwarding.example", pending.OrderRef)
	if len(order.Order.Legs) != 2 {
		t.Fatalf("legs %+v", order.Order.Legs)
	}
	ship, truck := order.Order.Legs[0], order.Order.Legs[1]
	if _, err := meridian.Customer.Bookings.Get("ocean-blue.example", ship.Ref); err != nil {
		t.Errorf("the ocean leg: %v", err)
	}
	leg, err := meridian.Customer.Bookings.Get("serra-transportes.example", truck.Ref)
	if err != nil || leg.Road == nil {
		t.Fatalf("the truck leg: %+v, %v", leg, err)
	}
	// The truck collects the ship's container, after the ship's arrival and
	// the handover, and delivers to the order's door by the order's date.
	arrival, _ := time.Parse(time.RFC3339, ship.Arrival)
	pickup, _ := time.Parse(time.RFC3339, leg.Road.Request.Pickup.Earliest)
	if !pickup.Equal(arrival.Add(forwarder.Handover)) || len(leg.Road.Request.EquipmentReferences) != 1 ||
		leg.Road.Request.EquipmentReferences[0] != ship.Containers[0] || leg.Road.Request.Delivery.Latest != ordered.PlaceOfDelivery.Latest {
		t.Errorf("the truck leg's request %+v", leg.Road.Request)
	}
	if _, err := meridian.Orchestrator.Book(ctx, pending.OrderRef); !errors.Is(err, forwarder.ErrAnswered) {
		t.Errorf("booking an answered order: %v", err)
	}
}

// TestDoorToDoorRefusals: an order on no lane can't be planned or booked,
// and nothing is booked for it.
func TestDoorToDoorRefusals(t *testing.T) {
	apps := startNodes(t, journeyRig)
	cymbal, meridian := apps["cymbal-shoes.example"], apps["meridian-forwarding.example"]
	ctx := context.Background()
	o := portOrder()
	o.PlaceOfReceipt.UNLocationCode = "DEHAM"
	pending, err := cymbal.Shipper.Order(ctx, "meridian-forwarding.example", o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := meridian.Orchestrator.Plan(pending.OrderRef); !errors.Is(err, forwarder.ErrNoLane) {
		t.Errorf("plan: %v", err)
	}
	if _, err := meridian.Orchestrator.Book(ctx, pending.OrderRef); !errors.Is(err, forwarder.ErrNoLane) || meridian.Customer.Bookings.Len() != 0 {
		t.Errorf("book: %v", err)
	}
	if _, err := meridian.Orchestrator.Plan("MF-9999"); !errors.Is(err, forwarder.ErrUnknownOrder) {
		t.Errorf("an unknown order: %v", err)
	}
}
