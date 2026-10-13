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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/business"
	"github.com/go-steer/supercargo/internal/config"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
)

// TestBusinessContextNeverLeaves: a shipper node with a business file
// refuses to send any envelope carrying its private text, and a broken
// file stops the node.
func TestBusinessContextNeverLeaves(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "business.yaml")
	_ = os.WriteFile(file, []byte(`shipments:
  - shipperRef: CY-PO-55120
    requiredBy: 2026-10-15
    receivingSlot: { date: 2026-10-15, window: "08:00-12:00", warehouse: "Newark DC dock 4" }
`), 0o600)
	c := cfg(t, config.RoleShipper)
	c.Business = &config.Business{File: file}
	a, err := New(c, quiet())
	if err != nil {
		t.Fatal(err)
	}
	if s, ok := a.Business.For("CY-PO-55120"); !ok || s.RequiredBy != "2026-10-15" {
		t.Errorf("business context %+v", a.Business)
	}
	h := envelope.NewHeader(intents.TnTInquiry, "cymbal-shoes.example", "ocean-blue.example", time.Now())
	ref := "OB-BKG-1"
	h.BookingRef = &ref
	_, err = a.Node.Exchange(context.Background(), h, &intents.TnTInquiryBody{Question: "Can you deliver to Newark DC dock 4 by Thursday?"},
		intents.TnTInquiryResponse)
	if !errors.Is(err, business.ErrLeak) {
		t.Errorf("an inquiry carrying the warehouse: %v, want ErrLeak", err)
	}
	// So does a truck leg's request, or a forwarding order.
	leg := &intents.RoadBookingRequestBody{ShipperRef: "CY-L3", Shipper: intents.Party{Name: "Cymbal Shoes"},
		Pickup:    intents.Stop{Facility: "Port Newark terminal", UNLocationCode: "USEWR"},
		Delivery:  intents.Stop{Facility: "Newark DC dock 4", UNLocationCode: "USNYC"},
		Equipment: []intents.Equipment{{ISOType: "45G1", Count: 1, GrossWeightKg: 12000}}, Cargo: intents.Cargo{Description: "Shoes", HSCode: "6404"}}
	if _, err := a.Node.Exchange(context.Background(), intents.HeaderFor(intents.RoadBookingRequest, "cymbal-shoes.example", "trucks.example", time.Now()),
		leg, intents.RoadBookingConfirmation); !errors.Is(err, business.ErrLeak) {
		t.Errorf("a truck leg to the warehouse: %v, want ErrLeak", err)
	}
	order := &intents.ForwardingRequestBody{ShipperRef: "CY-PO-55120", Incoterms: intents.Incoterms{Rule: "DAP", Version: "2020", NamedPlace: "Newark DC dock 4"},
		Shipper: intents.Party{Name: "Cymbal Shoes"}, Consignee: intents.Party{Name: "Cymbal Footwear"},
		PlaceOfReceipt:  intents.Place{Type: intents.PlacePort, Facility: "Euromax", UNLocationCode: "NLRTM"},
		PlaceOfDelivery: intents.Place{Type: intents.PlacePort, Facility: "Port Newark", UNLocationCode: "USEWR"},
		Equipment:       []intents.Equipment{{ISOType: "45G1", Count: 1, GrossWeightKg: 12000}}, Cargo: intents.Cargo{Description: "Shoes", HSCode: "6404"}}
	if _, err := a.Node.Exchange(context.Background(), intents.HeaderFor(intents.ForwardingRequest, "cymbal-shoes.example", "forwarder.example", time.Now()),
		order); !errors.Is(err, business.ErrLeak) {
		t.Errorf("an order naming the warehouse: %v, want ErrLeak", err)
	}

	_ = os.WriteFile(file, []byte("secret: yes\n"), 0o600)
	if _, err := New(c, quiet()); err == nil {
		t.Error("a broken business file didn't stop the node")
	}
}
