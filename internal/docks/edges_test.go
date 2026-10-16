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
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/backend"
	"github.com/go-steer/supercargo/internal/business"
)

// TestMoveEdges: a slot sent in another offset is found on the site's own
// day; an appointment may move into a slot it overlaps; a slot without a
// window isn't an appointment; the longest dock named wins; the control
// route wants a whole move.
func TestMoveEdges(t *testing.T) {
	ctx := context.Background()
	tokyo := time.FixedZone("JST", 9*3600)
	m := &Mock{Site: "Tokyo DC", Docks: []string{"Dock 1", "Dock 12"}, Zone: tokyo, Now: func() time.Time { return time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC) }}
	if err := m.Seed(&business.Context{Shipments: []business.Shipment{
		{ShipperRef: "A", ReceivingSlot: &business.Slot{Date: "2026-11-10", Window: "06:00-10:00", Warehouse: "east, dock 12"}},
		{ShipperRef: "B", ReceivingSlot: &business.Slot{Date: "2026-11-10"}},
	}}); err != nil {
		t.Fatal(err)
	}
	a, _ := m.Appointment(ctx, "A")
	if a.Dock != "Dock 12" {
		t.Errorf("dock %q", a.Dock)
	}
	if _, err := m.Appointment(ctx, "B"); !errors.Is(err, ErrNoAppointment) {
		t.Errorf("no window: %v", err)
	}
	// 06:00 in Tokyo is 21:00Z the day before.
	slots, _ := m.Slots(ctx, time.Date(2026, 11, 11, 0, 0, 0, 0, tokyo), 1)
	first := slots[0]
	utc := backend.DockSlot{Dock: first.Dock, Start: first.Start.UTC(), End: first.End.UTC()}
	if _, err := m.Move(ctx, a.ID, a.Start, utc); err != nil {
		t.Errorf("a slot in UTC: %v", err)
	}
	// Into a slot overlapping its own time: that doesn't block it.
	a, _ = m.Appointment(ctx, "A")
	own := backend.DockSlot{Dock: a.Dock, Start: a.Start, End: a.End}
	if moved, err := m.Move(ctx, a.ID, a.Start, own); err != nil || !moved.Start.Equal(a.Start) {
		t.Errorf("its own slot: %v", err)
	}
	mux := http.NewServeMux()
	RegisterControl(mux, m)
	for _, body := range []string{
		`{"dock":"Dock 1","start":"2026-11-11T06:00:00+09:00","end":"2026-11-11T10:00:00+09:00"}`,
		`{"from":"2026-11-10T06:00:00+09:00","start":"2026-11-11T06:00:00+09:00","end":"2026-11-11T10:00:00+09:00"}`,
		`{"from":"2026-11-10T06:00:00+09:00","dock":"Dock 1","start":"2026-11-11T06:00:00+09:00","end":"2026-11-11T06:00:00+09:00"}`,
	} {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/docks/appointments/"+a.ID+"/move", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		mux.ServeHTTP(rec, r)
		if rec.Code != 400 {
			t.Errorf("%s: HTTP %d", body, rec.Code)
		}
	}
}
