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
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/pkg/intents"
)

// TestRoadCarrierFromConfig: an operator with a road section takes a road
// booking and an amendment over TLS, from a signed request through the
// whole pipeline, and its profile says so.
func TestRoadCarrierFromConfig(t *testing.T) {
	p := startPair(t, `, road: { transit: 30h, cte: { cnpj: "12345678000190", state: 35 } }`)
	ctx := context.Background()
	body := &intents.RoadBookingRequestBody{
		ShipperRef: "CY-L3",
		Shipper:    intents.Party{Name: "Cymbal Shoes", Domain: "cymbal-shoes.example"},
		Pickup:     intents.Stop{Facility: "Santos Brasil Tecon terminal", UNLocationCode: "BRSSZ"},
		Delivery:   intents.Stop{Facility: "Cymbal Calçados DC, dock 2", UNLocationCode: "BRSAO"},
		Equipment:  []intents.Equipment{{ISOType: "45G1", Count: 1, GrossWeightKg: 12000}},
		Cargo:      intents.Cargo{Description: "Footwear (sneakers)", HSCode: "6404"},
	}
	h := intents.HeaderFor(intents.RoadBookingRequest, "cymbal-shoes.example", "ocean-blue.example", time.Now())
	resp, err := p.shipper.Node.Exchange(ctx, h, body, intents.RoadBookingConfirmation)
	if err != nil {
		t.Fatal(err)
	}
	c := resp.Body.(*intents.RoadBookingConfirmationBody)
	// Ocean Blue books ocean too: its truck legs have their own numbers.
	if c.Status != intents.StatusConfirmed || len(c.FiscalDocuments) != 1 || *resp.Envelope.Header.BookingRef != c.BookingRef || c.BookingRef != "OB-BKG-R-0913" {
		t.Fatalf("confirmation %+v", c)
	}
	b, err := p.operator.Carrier.Bookings.Get("ocean-blue.example", c.BookingRef)
	if err != nil || b.Road == nil || b.Shipper != "cymbal-shoes.example" {
		t.Fatalf("stored %+v, %v", b, err)
	}
	// The shipper moves the pickup a day later.
	later := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Hour).Format("2006-01-02T15:04:05Z")
	ah := intents.HeaderFor(intents.RoadBookingAmendment, "cymbal-shoes.example", "ocean-blue.example", time.Now())
	ah.BookingRef = &c.BookingRef
	resp, err = p.shipper.Node.Exchange(ctx, ah, &intents.RoadBookingAmendmentBody{BookingRef: c.BookingRef, Pickup: &intents.Window{Earliest: later}}, intents.RoadBookingConfirmation)
	if err != nil {
		t.Fatal(err)
	}
	if a := resp.Body.(*intents.RoadBookingConfirmationBody); a.Status != intents.StatusConfirmed || a.PlannedPickup != later || a.FiscalDocuments[0] != c.FiscalDocuments[0] {
		t.Errorf("amended %+v", a)
	}
	// An amendment of a leg that doesn't exist is the same refusal as
	// someone else's.
	unknown := "OB-BKG-0001"
	ah = intents.HeaderFor(intents.RoadBookingAmendment, "cymbal-shoes.example", "ocean-blue.example", time.Now())
	ah.BookingRef = &unknown
	_, err = p.shipper.Node.Exchange(ctx, ah, &intents.RoadBookingAmendmentBody{BookingRef: unknown, Pickup: &intents.Window{Earliest: later}}, intents.RoadBookingConfirmation)
	var re *node.RemoteError
	if !errors.As(err, &re) || re.Code != intents.CodeForbidden {
		t.Errorf("an unknown leg: %v", err)
	}
	// The profile lists the road capability and dialect.
	doc, err := p.shipper.Keys.Profile(ctx, "ocean-blue.example")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := doc.Capability("dev.ulp.logistics.road"); !ok || !doc.SupportsDialect("ULP_ROAD") || !doc.Accepts(intents.RoadBookingAmendment) {
		t.Errorf("profile %+v", doc.Profile)
	}
}

// TestCustomerRoadLeg: a shipper books a truck leg through its node,
// records it, takes the carrier's signed events for it, and amends it.
func TestCustomerRoadLeg(t *testing.T) {
	p := startPair(t, `, road: { transit: 30h }`)
	ctx := context.Background()
	body := &intents.RoadBookingRequestBody{
		ShipperRef:          "CY-L3",
		Shipper:             intents.Party{Name: "Cymbal Shoes", Domain: "cymbal-shoes.example"},
		Pickup:              intents.Stop{Facility: "Santos Brasil Tecon terminal", UNLocationCode: "BRSSZ"},
		Delivery:            intents.Stop{Facility: "Cymbal Calçados DC, dock 2", UNLocationCode: "BRSAO"},
		Equipment:           []intents.Equipment{{ISOType: "45G1", Count: 1, GrossWeightKg: 12000}},
		EquipmentReferences: []string{"OCBU0000004"},
		Cargo:               intents.Cargo{Description: "Footwear (sneakers)", HSCode: "6404"},
		Subscription:        &intents.Subscription{Endpoint: "https://cymbal-shoes.example/ulp", Intents: []string{intents.TnTEvent, intents.TnTAlert}},
	}
	c, err := p.shipper.Shipper.BookRoad(ctx, "ocean-blue.example", body)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := p.shipper.Customer.Bookings.Get("ocean-blue.example", c.BookingRef); err != nil || b.Road == nil {
		t.Fatalf("not recorded: %+v, %v", b, err)
	}
	// The carrier's simulator picks the container up: its signed event is
	// accepted and logged on the shipper's side.
	deadline := time.Now().Add(15 * time.Second)
	for len(p.shipper.Customer.Events.List("ocean-blue.example", c.BookingRef)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no event for the truck leg")
		}
		time.Sleep(100 * time.Millisecond)
	}
	later := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Hour).Format("2006-01-02T15:04:05Z")
	a, err := p.shipper.Shipper.AmendRoad(ctx, "ocean-blue.example", c.BookingRef, &intents.RoadBookingAmendmentBody{Delivery: &intents.Window{Earliest: later}})
	if err != nil || a.Status != intents.StatusConfirmed || a.PlannedDelivery != later {
		t.Fatalf("amended: %+v, %v", a, err)
	}
	if b, _ := p.shipper.Customer.Bookings.Get("ocean-blue.example", c.BookingRef); b.Road.Request.Delivery.Earliest != later || b.Road.Confirmation.PlannedDelivery != later {
		t.Errorf("recorded %+v", b.Road)
	}
	if _, err := p.shipper.Shipper.AmendRoad(ctx, "ocean-blue.example", "OB-BKG-R-9999", &intents.RoadBookingAmendmentBody{Delivery: &intents.Window{Earliest: later}}); err == nil {
		t.Error("amended a leg we don't hold")
	}
	if _, err := p.shipper.Shipper.Status(ctx, "ocean-blue.example", c.BookingRef); err == nil {
		t.Error("an ocean status request for a truck leg")
	}
}
