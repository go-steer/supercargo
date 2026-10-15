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

package chatui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/go-steer/supercargo/internal/assistant"
	"github.com/go-steer/supercargo/pkg/intents"
)

// TestOrderDialog: an order draft's dialog shows what the forwarder gets,
// door to door, and what stays home; its outcome is the forwarder's answer.
func TestOrderDialog(t *testing.T) {
	m := newModel(t, assistant.FakeModel(time.Now), "dark")
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	o := intents.ForwardingRequestBody{ShipperRef: "CY-PO-55120", Incoterms: intents.Incoterms{Rule: "DAP", Version: "2020", NamedPlace: "Cajamar DC"},
		Shipper:   intents.Party{Name: "Cymbal Shoes B.V.", Identifiers: []intents.Identifier{{Scheme: "EORI", Value: "NL123456789"}}},
		Consignee: intents.Party{Name: "Cymbal Calçados Ltda"},
		PlaceOfReceipt: intents.Place{Type: intents.PlaceDoor, UNLocationCode: "NLTLB", Earliest: "2026-10-16T06:00:00Z",
			Address: &intents.Address{Street: "Ringbaan Oost 100", Locality: "Tilburg", Country: "NL"}},
		PlaceOfDelivery: intents.Place{Type: intents.PlaceDoor, Facility: "Cymbal Calçados DC, dock 2", UNLocationCode: "BRSAO", Latest: "2026-11-05T12:00:00Z",
			Address: &intents.Address{Street: "Rodovia Anhanguera, km 33", Locality: "Cajamar", Country: "BR"}},
		Equipment: []intents.Equipment{{ISOType: "L5G1", Count: 1, GrossWeightKg: 12000}}, Cargo: intents.Cargo{Description: "Footwear (sneakers)", HSCode: "6404"}}
	o.Subscription = &intents.Subscription{Endpoint: "https://cymbal-shoes.example/ulp", Intents: []string{intents.TnTEvent}}
	m.offer(assistant.DraftResult{DraftID: "OD-0001", To: "meridian-forwarding.example", Order: &o, NewPartner: true,
		NotSent: map[string]string{"requiredBy": "2026-11-07", "receivingSlot": "2026-11-07 08:00-12:00, Brazil launch stock"}})
	s := screen(m)
	for _, want := range []string{"Order draft OD-0001", "meridian-forwarding.example", "Ringbaan Oost 100", "Cymbal Calçados DC, dock 2",
		"A new partner", "https://cymbal-shoes.example/ulp",
		"2026-11-05T12:00:00Z", "NL123456789", "Footwear (sneakers), HS 6404", "your deadline, 2026-11-07", "your receiving slot", "Send this order?"} {
		if !strings.Contains(s, want) {
			t.Errorf("the dialog lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "booking request") {
		t.Errorf("an order shown as a booking:\n%s", s)
	}
	// The forwarder's answer: shown, without a booking's confirmation.
	m.sent(sentMsg{id: "OD-0001", sent: &assistant.Sent{Draft: assistant.Draft{ID: "OD-0001", Order: &assistant.OrderDraft{Forwarder: "meridian-forwarding.example"}},
		Ordered: &intents.ForwardingConfirmationBody{OrderRef: "MF-0007", Status: "pending"}}})
	if last := m.rows[len(m.rows)-1].Text; last != "Sent order OD-0001 to meridian-forwarding.example, signed. Answer: pending MF-0007." {
		t.Errorf("outcome %q", last)
	}
	// Sent with no answer to show: said plainly.
	m.sent(sentMsg{id: "OD-0002", sent: &assistant.Sent{Draft: assistant.Draft{ID: "OD-0002"}}})
	if last := m.rows[len(m.rows)-1].Text; last != "Sent OD-0002, signed." {
		t.Errorf("no answer: %q", last)
	}
	// An order whose outcome is unknown: the assistant is told it may have
	// reached the forwarder.
	m.sent(sentMsg{id: "OD-0003", err: assistant.ErrOutcomeUnknown})
	if !strings.Contains(m.rows[len(m.rows)-1].Text, "OD-0003") {
		t.Errorf("unknown outcome: %q", m.rows[len(m.rows)-1].Text)
	}
}
