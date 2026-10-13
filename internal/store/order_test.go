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
	"testing"

	"github.com/go-steer/supercargo/pkg/intents"
)

func orderBooking() Booking {
	return Booking{Ref: "MF-0007", Shipper: "cymbal-shoes.example", Operator: "meridian-forwarding.example",
		Order: &OrderBooking{MessageID: "m1",
			Request: intents.ForwardingRequestBody{ShipperRef: "CY-PO-55120",
				Shipper:         intents.Party{Name: "Cymbal", Address: &intents.Address{Street: "Ringbaan Oost 100", Locality: "Tilburg", Country: "NL"}},
				PlaceOfReceipt:  intents.Place{Type: intents.PlaceDoor, UNLocationCode: "NLTLB"},
				PlaceOfDelivery: intents.Place{Type: intents.PlaceDoor, UNLocationCode: "BRSAO"},
				Subscription:    &intents.Subscription{Endpoint: "https://cymbal-shoes.example/ulp", Intents: []string{intents.TnTEvent}}},
			Confirmation: intents.ForwardingConfirmationBody{OrderRef: "MF-0007", Status: intents.StatusConfirmed, SubscriptionID: "SUB-MF-0007",
				PlannedDelivery: "2026-10-26T10:00:00Z",
				Legs:            []intents.PlannedLeg{{Mode: intents.ModeRoad, From: "NLTLB", To: "BRSAO", PlannedDeparture: "2026-10-06T08:00:00Z"}}}}}
}

// TestOrderViews: an order's views, summary and journey read the order:
// its places, its subscription and its planned delivery; it names no
// containers; it is copied deeply.
func TestOrderViews(t *testing.T) {
	b := orderBooking()
	if from, to := b.Places(); from != "NLTLB" || to != "BRSAO" || b.Subscription() != b.Order.Request.Subscription ||
		b.SubscriptionID() != "SUB-MF-0007" || b.EquipmentRefs() != nil || b.ShipperRef() != "CY-PO-55120" {
		t.Error("order views")
	}
	s := Summarize(b, nil)
	if s.Status != intents.StatusConfirmed || s.ETA != "2026-10-26T10:00:00Z" || s.PlannedDeparture != "2026-10-06T08:00:00Z" || s.Origin.UNLocode != "NLTLB" {
		t.Errorf("summary %+v", s)
	}
	j := BuildJourney(b, nil)
	if len(j.Legs) != 1 || j.Legs[0].Departure.At != "2026-10-06T08:00:00Z" || j.Legs[0].Arrival.At != "2026-10-26T10:00:00Z" {
		t.Errorf("journey %+v", j.Legs)
	}
	st := NewBookings()
	_ = st.Add(b)
	b.Order.Request.Shipper.Address.Street = "changed"
	if got, _ := st.Get(b.Operator, b.Ref); got.Order.Request.Shipper.Address.Street != "Ringbaan Oost 100" {
		t.Error("the caller changed the stored order")
	}
	// A pending order: no plan yet.
	b.Order.Confirmation = intents.ForwardingConfirmationBody{OrderRef: "MF-0007", Status: intents.StatusPending}
	if s := Summarize(b, nil); s.Status != intents.StatusPending || s.ETA != "" || s.PlannedDeparture != "" {
		t.Errorf("pending summary %+v", s)
	}
}
