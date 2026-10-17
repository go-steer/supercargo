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

package assistant

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/go-steer/supercargo/internal/llm"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/intents"
)

// TestShareWhich: a reference two carriers' bookings share needs the
// carrier; a cancelled booking or one we follow by delegation can't be
// shared.
func TestShareWhich(t *testing.T) {
	n := &truckNode{node: node{shipments: []store.Summary{
		{Operator: "serra-transportes.example", Shipper: "vale-verde.example", Ref: "R-1", Kind: store.KindRoad},
		{Operator: "other-trucks.example", Shipper: "vale-verde.example", Ref: "R-1", Kind: store.KindRoad},
		{Operator: "serra-transportes.example", Shipper: "vale-verde.example", Ref: "R-2", Kind: store.KindRoad, Status: intents.StatusCancelled},
		{Operator: "serra-transportes.example", Shipper: "vale-verde.example", Ref: "R-3", Kind: store.KindRoad, DelegatedBy: "x.example"},
	}}}
	a, c := truckAssistant(n)
	for name, raw := range map[string]string{
		"ambiguous": `{"bookingRef":"R-1","grantee":"mercado-aurora.example","days":7}`,
		"cancelled": `{"bookingRef":"R-2","grantee":"mercado-aurora.example","days":7}`,
		"delegated": `{"bookingRef":"R-3","grantee":"mercado-aurora.example","days":7}`,
	} {
		if _, err := a.draftGrant(context.Background(), c, json.RawMessage(raw)); err == nil {
			t.Errorf("%s: drafted", name)
		}
	}
	out, err := a.draftGrant(context.Background(), c, json.RawMessage(`{"bookingRef":"R-1","carrier":"other-trucks.example","grantee":"mercado-aurora.example","days":7}`))
	if err != nil || out.(DraftResult).Grant.Carrier != "other-trucks.example" {
		t.Errorf("with the carrier: %+v %v", out, err)
	}
}

// TestDraftDeadlines: an open draft's private deadline is as private as
// the business file's, for a truckload and an order.
func TestDraftDeadlines(t *testing.T) {
	a, c := truckAssistant(&truckNode{})
	_, _ = a.Drafts.add(Draft{Operator: "ocean-blue.example", RequiredBy: "2026-10-22", Request: intents.BookingRequestBody{ShipperRef: "X"}})
	if _, err := a.run(context.Background(), c, &turnState{}, llm.ToolCall{Name: ToolDraftRoad, Input: json.RawMessage(truckInput)}); err == nil || !strings.Contains(err.Error(), "2026-10-22") {
		t.Errorf("a truckload by an open draft's deadline: %v", err)
	}
}

// TestReceiverTools: a node that can't book offers no booking tools.
func TestReceiverTools(t *testing.T) {
	a, _ := truckAssistant(&truckNode{})
	a.NoBooking = true
	for _, tool := range a.tools() {
		if slices.Contains([]string{ToolDraftBooking, ToolDraftOrder, ToolDraftRoad}, tool.Name) {
			t.Errorf("a receiver offers %s", tool.Name)
		}
	}
}

// TestPrivatePickup: a pickup on a private date is refused, the rest of
// the truckload being fine.
func TestPrivatePickup(t *testing.T) {
	a, c := truckAssistant(&truckNode{})
	a.Private = []string{"2026-10-20"}
	if _, err := a.run(context.Background(), c, &turnState{}, llm.ToolCall{Name: ToolDraftRoad, Input: json.RawMessage(truckInput)}); err == nil || !strings.Contains(err.Error(), "2026-10-20") {
		t.Errorf("a private pickup: %v", err)
	}
}
