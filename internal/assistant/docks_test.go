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
	"time"

	"github.com/go-steer/supercargo/internal/backend"
	"github.com/go-steer/supercargo/internal/business"
	"github.com/go-steer/supercargo/internal/docks"
	"github.com/go-steer/supercargo/internal/llm"
)

// dockSchedule is the mock schedule as the node serves it.
type dockSchedule struct {
	m       *docks.Mock
	moveErr error
	moves   int
}

func (d *dockSchedule) DockAppointment(ctx context.Context, ref string) (*backend.Appointment, error) {
	return d.m.Appointment(ctx, ref)
}
func (d *dockSchedule) DockSlots(ctx context.Context, from time.Time, days int) ([]backend.DockSlot, error) {
	return d.m.Slots(ctx, from, days)
}
func (d *dockSchedule) MoveDock(ctx context.Context, id string, from time.Time, to backend.DockSlot) (*backend.Appointment, error) {
	d.moves++
	if d.moveErr != nil {
		return nil, d.moveErr
	}
	return d.m.Move(ctx, id, from, to)
}

func dockAssistant(t *testing.T) (*Assistant, *dockSchedule) {
	t.Helper()
	m := &docks.Mock{Source: "Agendamento (simulated)", Site: "Cajamar DC", Docks: []string{"Dock 1", "Dock 2"}, Zone: time.FixedZone("BRT", -3*3600),
		Now: func() time.Time { return thursday }}
	if err := m.Seed(&business.Context{Shipments: []business.Shipment{{ShipperRef: "CY-PO-55120",
		ReceivingSlot: &business.Slot{Date: thursday.AddDate(0, 0, 7).Format(time.DateOnly), Window: "08:00-12:00", Warehouse: "dock 2"}}}}); err != nil {
		t.Fatal(err)
	}
	d := &dockSchedule{m: m}
	return &Assistant{Node: &node{}, Docks: d, Drafts: &Drafts{}, Own: true, Now: func() time.Time { return thursday }}, d
}

func (a *Assistant) tool(t *testing.T, name, raw string) (any, error) {
	t.Helper()
	return a.run(context.Background(), &Conversation{}, &turnState{}, llm.ToolCall{Name: name, Input: json.RawMessage(raw)})
}

// TestDockMove: the appointment and the free slots are read; a move is
// drafted to a free slot and made only by the dialog's send; nothing of
// it is sent to anyone.
func TestDockMove(t *testing.T) {
	a, d := dockAssistant(t)
	out, err := a.tool(t, ToolGetDock, `{"shipperRef":"CY-PO-55120"}`)
	appt, _ := out.(*backend.Appointment)
	if err != nil || appt == nil || appt.Dock != "Dock 2" {
		t.Fatalf("%+v %v", out, err)
	}
	day := thursday.AddDate(0, 0, 10).Format(time.DateOnly)
	out, err = a.tool(t, ToolFindSlots, `{"from":"`+day+`","days":2}`)
	if err != nil {
		t.Fatal(err)
	}
	slots := out.(map[string]any)["slots"].([]backend.DockSlot)
	if len(slots) == 0 {
		t.Fatal("no slots")
	}
	s := slots[0]
	out, err = a.tool(t, ToolDraftDock, `{"shipperRef":"CY-PO-55120","dock":"`+s.Dock+`","start":"`+s.Start.Format(time.RFC3339)+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	dr := out.(DraftResult)
	if dr.Dock == nil || !strings.HasPrefix(dr.DraftID, "AD-") || dr.Dock.WasDock != "Dock 2" || !dr.Dock.Start.Equal(s.Start) || d.moves != 0 {
		t.Fatalf("draft %+v, %d moves", dr, d.moves)
	}
	sent, err := a.Send(context.Background(), dr.DraftID)
	if err != nil || sent.Docked == nil || !sent.Docked.Start.Equal(s.Start) || d.moves != 1 {
		t.Fatalf("sent %+v %v", sent, err)
	}
	now, _ := d.m.Appointment(context.Background(), "CY-PO-55120")
	if !now.Start.Equal(s.Start) {
		t.Errorf("not moved: %+v", now)
	}
}

// TestDockRefusals: a slot that isn't free, a bad time or day, an
// unknown shipment: nothing drafted; a refused move puts the draft back.
func TestDockRefusals(t *testing.T) {
	a, d := dockAssistant(t)
	appt, _ := d.m.Appointment(context.Background(), "CY-PO-55120")
	for name, tc := range map[string][2]string{
		"its own slot":  {ToolDraftDock, `{"shipperRef":"CY-PO-55120","dock":"Dock 2","start":"` + appt.Start.Format(time.RFC3339) + `"}`},
		"not a time":    {ToolDraftDock, `{"shipperRef":"CY-PO-55120","dock":"Dock 1","start":"tomorrow"}`},
		"unknown":       {ToolDraftDock, `{"shipperRef":"CY-PO-9","dock":"Dock 1","start":"` + appt.Start.Format(time.RFC3339) + `"}`},
		"not JSON":      {ToolDraftDock, `[`},
		"a bad day":     {ToolFindSlots, `{"from":"next week","days":1}`},
		"too many days": {ToolFindSlots, `{"from":"2026-11-01","days":9}`},
	} {
		if _, err := a.tool(t, tc[0], tc[1]); err == nil {
			t.Errorf("%s: done", name)
		}
	}
	if len(a.Drafts.Open()) != 0 {
		t.Fatal("a draft was made")
	}
	slots, _ := d.m.Slots(context.Background(), thursday.AddDate(0, 0, 10), 1)
	out, err := a.tool(t, ToolDraftDock, `{"shipperRef":"CY-PO-55120","dock":"`+slots[0].Dock+`","start":"`+slots[0].Start.Format(time.RFC3339)+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	d.moveErr = &NodeError{Status: 409, Message: "taken"}
	if _, err := a.Send(context.Background(), out.(DraftResult).DraftID); err == nil || len(a.Drafts.Open()) != 1 {
		t.Errorf("a refused move: %v, %d open", err, len(a.Drafts.Open()))
	}
	a.Docks = nil
	if _, err := a.Send(context.Background(), out.(DraftResult).DraftID); err == nil || !strings.Contains(err.Error(), "no dock schedule") {
		t.Errorf("no schedule: %v", err)
	}
	if _, err := a.tool(t, ToolGetDock, `{"shipperRef":"CY-PO-55120"}`); err == nil || !strings.Contains(err.Error(), "no tool") {
		t.Errorf("without a schedule: %v", err)
	}
}

// TestDockTools: only the shipper's own assistant with a schedule has
// them, and its rules say the schedule stays ours.
func TestDockTools(t *testing.T) {
	a, _ := dockAssistant(t)
	has := func(a *Assistant) bool {
		return slices.ContainsFunc(a.tools(), func(x llm.Tool) bool { return x.Name == ToolDraftDock })
	}
	plain := &Assistant{Node: &node{}, Drafts: &Drafts{}}
	if !has(a) || has(plain) || !strings.Contains(a.system(), "Appointments are ours") || strings.Contains(plain.system(), "dock") {
		t.Error("the dock tools or rules")
	}
	var c Conversation
	sent := &Sent{Draft: Draft{ID: "AD-0001"}, Docked: &backend.Appointment{ID: "AG-1", Dock: "Dock 1"}}
	a.Model = FakeModel(func() time.Time { return thursday })
	if _, err := a.Report(context.Background(), &c, sent); err != nil {
		t.Errorf("report: %v", err)
	}
}
