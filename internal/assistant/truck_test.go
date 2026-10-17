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
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/backend"
	"github.com/go-steer/supercargo/internal/docks"
	"github.com/go-steer/supercargo/internal/llm"
	"github.com/go-steer/supercargo/internal/store"
)

// TestStatusOfATruck: get_status on a truck leg, or a booking followed by
// delegation, asks nobody: it answers with what we hold and where to
// look, not a failure.
func TestStatusOfATruck(t *testing.T) {
	for name, s := range map[string]store.Summary{
		"a truck leg":         {Operator: "serra.example", Ref: "SR-1", Kind: store.KindRoad},
		"a delegated booking": {Operator: "ocean.example", Ref: "OB-1", Kind: store.KindOcean, DelegatedBy: "vv.example"},
	} {
		n := &node{shipments: []store.Summary{s}}
		a := &Assistant{Node: n, Drafts: &Drafts{}}
		out, err := a.run(context.Background(), &Conversation{}, &turnState{statused: map[string]bool{}}, llm.ToolCall{Name: ToolStatus, Input: json.RawMessage(`{"bookingRef":"` + s.Ref + `"}`)})
		raw, _ := json.Marshal(out)
		if err != nil || !strings.Contains(string(raw), "get_events") || !strings.Contains(string(raw), `"bookingRef":"`+s.Ref+`"`) || strings.Contains(strings.Join(n.calls, ","), "status") {
			t.Errorf("%s: %s %v, asked %v", name, raw, err, n.calls)
		}
	}
}

// TestDockOfATruck: the dock tools find a truck we follow by delegation
// by its carrier and reference, never one of our own shipments of the
// same reference.
func TestDockOfATruck(t *testing.T) {
	a, d := dockAssistant(t)
	a.Node = &node{shipments: []store.Summary{{Operator: "serra.example", Ref: "CY-PO-55120", Kind: store.KindRoad, DelegatedBy: "vv.example"}}}
	free, _ := d.m.Slots(context.Background(), thursday.AddDate(0, 0, 3), 1)
	if _, err := d.m.Book(context.Background(), docks.TruckKey("serra.example", "CY-PO-55120"), "policy", free[0]); err != nil {
		t.Fatal(err)
	}
	out, err := a.tool(t, ToolGetDock, `{"shipperRef":"CY-PO-55120"}`)
	appt, _ := out.(*backend.Appointment)
	if err != nil || appt == nil || appt.ShipperRef != "serra.example/CY-PO-55120" || !appt.Start.Equal(free[0].Start) {
		t.Fatalf("%+v %v", appt, err)
	}
	// Without the delegation, it's our own shipment's.
	a.Node = &node{}
	out, _ = a.tool(t, ToolGetDock, `{"shipperRef":"CY-PO-55120"}`)
	if appt, _ := out.(*backend.Appointment); appt == nil || appt.ShipperRef != "CY-PO-55120" || appt.Start.Equal(free[0].Start) {
		t.Errorf("our own: %+v", appt)
	}
	// A draft for the truck moves the truck's appointment.
	a.Node = &node{shipments: []store.Summary{{Operator: "serra.example", Ref: "CY-PO-55120", Kind: store.KindRoad, DelegatedBy: "vv.example"}}}
	later, _ := d.m.Slots(context.Background(), free[0].Start.AddDate(0, 0, 1), 1)
	out, err = a.tool(t, ToolDraftDock, `{"shipperRef":"CY-PO-55120","dock":"`+later[0].Dock+`","start":"`+later[0].Start.Format(time.RFC3339)+`"}`)
	dr, _ := out.(DraftResult)
	if err != nil || dr.Dock == nil || dr.Dock.ShipperRef != "serra.example/CY-PO-55120" || !dr.Dock.WasStart.Equal(free[0].Start) {
		t.Errorf("a truck's draft: %+v %v", dr, err)
	}
}

// TestDockRefAmbiguous: a reference two carriers' trucks share, or one
// that's both our shipment and a truck, needs the carrier; a shipments
// list that fails is an error, not a guess.
func TestDockRefAmbiguous(t *testing.T) {
	ctx := context.Background()
	a := &Assistant{Node: &node{shipments: []store.Summary{
		{Operator: "a.example", Ref: "R1", DelegatedBy: "vv.example"},
		{Operator: "b.example", Ref: "R1", DelegatedBy: "vv.example"},
		{Operator: "c.example", Ref: "C-BKG-1", ShipperRef: "PO-7"},
		{Operator: "d.example", Ref: "PO-7", DelegatedBy: "vv.example"},
	}}}
	for _, c := range []struct{ ref, operator, want string }{
		{"R1", "", ""},
		{"R1", "b.example", "b.example/R1"},
		{"PO-7", "", ""},
		{"PO-7", "d.example", "d.example/PO-7"},
		{"PO-9", "", "PO-9"},
	} {
		got, err := a.dockRef(ctx, c.ref, c.operator)
		if got != c.want || (c.want == "") != (err != nil) {
			t.Errorf("%s/%s: %q %v", c.operator, c.ref, got, err)
		}
	}
	if _, err := (&Assistant{Node: &failingShipments{}}).dockRef(ctx, "R1", ""); err == nil {
		t.Error("a failing shipments list")
	}
}

// failingShipments is a node whose shipments list fails.
type failingShipments struct{ node }

func (*failingShipments) Shipments(context.Context) ([]store.Summary, error) {
	return nil, errors.New("down")
}

// TestStatusOfATruckOnce: a second get_status on a truck in one turn is
// refused, as any booking's.
func TestStatusOfATruckOnce(t *testing.T) {
	a := &Assistant{Node: &node{shipments: []store.Summary{{Operator: "serra.example", Ref: "SR-1", Kind: store.KindRoad}}}, Drafts: &Drafts{}}
	turn := &turnState{statused: map[string]bool{}}
	call := llm.ToolCall{Name: ToolStatus, Input: json.RawMessage(`{"bookingRef":"SR-1"}`)}
	if _, err := a.run(context.Background(), &Conversation{}, turn, call); err != nil {
		t.Fatal(err)
	}
	if _, err := a.run(context.Background(), &Conversation{}, turn, call); err == nil {
		t.Error("asked twice")
	}
}
