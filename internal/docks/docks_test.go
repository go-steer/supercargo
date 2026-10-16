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

package docks

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/backend"
	"github.com/go-steer/supercargo/internal/business"
)

var saoPaulo = time.FixedZone("BRT", -3*3600)

// cajamar is Cymbal's DC: three docks, the launch stock booked at dock 2
// on the 8th, 08:00-12:00; "now" is the 1st.
func cajamar(t *testing.T) *Mock {
	t.Helper()
	m := &Mock{Source: "Agendamento (simulated)", Site: "Cajamar DC", Docks: []string{"Dock 1", "Dock 2", "Dock 3"}, Zone: saoPaulo,
		Now: func() time.Time { return time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC) }}
	err := m.Seed(&business.Context{Shipments: []business.Shipment{
		{ShipperRef: "CY-PO-55120", ReceivingSlot: &business.Slot{Date: "2026-11-08", Window: "08:00-12:00", Warehouse: "Brazil launch stock, dock 2"}},
		{ShipperRef: "CY-PO-1", RequiredBy: "2026-12-01"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestAppointmentAndSlots: the seeded appointment, and free slots that
// never include it, a past one, or the same each run.
func TestAppointmentAndSlots(t *testing.T) {
	ctx := context.Background()
	m := cajamar(t)
	a, err := m.Appointment(ctx, "CY-PO-55120")
	if err != nil || a.Dock != "Dock 2" || !a.Start.Equal(time.Date(2026, 11, 8, 11, 0, 0, 0, time.UTC)) || a.Source != "Agendamento (simulated)" {
		t.Fatalf("%+v %v", a, err)
	}
	if _, err := m.Appointment(ctx, "CY-PO-1"); !errors.Is(err, ErrNoAppointment) {
		t.Errorf("no slot: %v", err)
	}
	slots, err := m.Slots(ctx, time.Date(2026, 11, 8, 0, 0, 0, 0, saoPaulo), 2)
	if err != nil || len(slots) == 0 || len(slots) >= 2*len(Windows)*3 {
		t.Fatalf("%d slots, %v", len(slots), err)
	}
	for i, s := range slots {
		if s.Dock == "Dock 2" && s.Start.Equal(a.Start) {
			t.Error("the appointment's own slot is free")
		}
		if i > 0 && s.Start.Before(slots[i-1].Start) {
			t.Error("not earliest first")
		}
	}
	again, _ := m.Slots(ctx, time.Date(2026, 11, 8, 0, 0, 0, 0, saoPaulo), 2)
	if len(again) != len(slots) {
		t.Error("not the same each run")
	}
	// None before now.
	past, _ := m.Slots(ctx, time.Date(2026, 10, 30, 0, 0, 0, 0, saoPaulo), 3)
	for _, s := range past {
		if !s.Start.After(m.Now()) {
			t.Errorf("a past slot %v", s.Start)
		}
	}
	for _, days := range []int{0, MaxDays + 1} {
		if _, err := m.Slots(ctx, m.Now(), days); err == nil {
			t.Errorf("%d days", days)
		}
	}
}

// TestMove: to a free slot only; the old one frees up.
func TestMove(t *testing.T) {
	ctx := context.Background()
	m := cajamar(t)
	a, _ := m.Appointment(ctx, "CY-PO-55120")
	slots, _ := m.Slots(ctx, time.Date(2026, 11, 11, 0, 0, 0, 0, saoPaulo), 1)
	to := slots[0]
	to.Start, to.End = to.Start.UTC(), to.End.UTC() // as decoded from JSON
	moved, err := m.Move(ctx, a.ID, a.Start, to)
	if err != nil || !moved.Start.Equal(to.Start) || moved.Dock != to.Dock {
		t.Fatalf("%+v %v", moved, err)
	}
	if _, err := m.Move(ctx, a.ID, a.Start, to); !errors.Is(err, ErrMoved) {
		t.Errorf("a stale move: %v", err)
	}
	if _, err := m.Move(ctx, "AG-0", a.Start, to); !errors.Is(err, ErrNoAppointment) {
		t.Errorf("unknown: %v", err)
	}
	odd := backend.DockSlot{Dock: "Dock 1", Start: to.Start.Add(time.Hour), End: to.End.Add(time.Hour)}
	if _, err := m.Move(ctx, a.ID, moved.Start, odd); !errors.Is(err, ErrSlotTaken) {
		t.Errorf("not a slot: %v", err)
	}
	if err := (&Mock{Docks: []string{"D"}, Zone: saoPaulo}).Seed(&business.Context{Shipments: []business.Shipment{
		{ShipperRef: "X", ReceivingSlot: &business.Slot{Date: "2026-11-08", Window: "12:00-08:00"}}}}); err == nil {
		t.Error("a backwards window seeded")
	}
}

// TestControl: the routes and their status codes.
func TestControl(t *testing.T) {
	m := cajamar(t)
	mux := http.NewServeMux()
	RegisterControl(mux, m)
	do := func(method, path, body string) (int, string) {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		mux.ServeHTTP(rec, r)
		return rec.Code, rec.Body.String()
	}
	if code, body := do("GET", "/docks/appointments/CY-PO-55120", ""); code != 200 || !strings.Contains(body, `"dock":"Dock 2"`) {
		t.Errorf("HTTP %d %s", code, body)
	}
	if code, _ := do("GET", "/docks/appointments/NOPE", ""); code != 404 {
		t.Errorf("unknown: HTTP %d", code)
	}
	code, body := do("GET", "/docks/slots?from=2026-11-11&days=1", "")
	var s SlotsResponse
	_ = json.Unmarshal([]byte(body), &s)
	if code != 200 || len(s.Slots) == 0 {
		t.Fatalf("slots: HTTP %d %s", code, body)
	}
	for _, q := range []string{"from=x&days=1", "from=2026-11-11", "from=2026-11-11&days=99"} {
		if code, _ := do("GET", "/docks/slots?"+q, ""); code != 400 {
			t.Errorf("%s: HTTP %d", q, code)
		}
	}
	move, _ := json.Marshal(MoveRequest{From: time.Date(2026, 11, 8, 8, 0, 0, 0, saoPaulo), Dock: s.Slots[0].Dock, Start: s.Slots[0].Start, End: s.Slots[0].End})
	if code, body := do("POST", "/docks/appointments/AG-004100/move", string(move)); code != 200 {
		t.Errorf("move: HTTP %d %s", code, body)
	}
	if code, _ := do("POST", "/docks/appointments/AG-004100/move", string(move)); code != 409 {
		t.Errorf("move again: HTTP %d", code)
	}
	if code, _ := do("POST", "/docks/appointments/AG-004100/move", "{"); code != 400 {
		t.Errorf("bad body: HTTP %d", code)
	}
	if status(errors.New("x")) != http.StatusBadGateway {
		t.Error("other errors")
	}
}
