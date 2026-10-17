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

package operator

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/intents"
)

func saoPaulo(t *testing.T) *Curfew {
	t.Helper()
	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		t.Fatal(err)
	}
	return &Curfew{Zone: "São Paulo truck restriction", LOCODEs: []string{"BRSAO", "BRGRU"}, From: 5 * 60, To: 21 * 60, Loc: loc,
		Yard: "Guarulhos staging yard", YardToDelivery: 90 * time.Minute, By: "Dispatch lead"}
}

// TestCurfew: an arrival inside the window waits for its end, plus the
// yard to the delivery; one outside it, or elsewhere, isn't touched.
func TestCurfew(t *testing.T) {
	c := saoPaulo(t)
	leg := store.Booking{Road: &store.RoadBooking{Request: intents.RoadBookingRequestBody{Delivery: intents.Stop{UNLocationCode: "BRSAO"}}}}
	at := time.Date(2026, 10, 26, 8, 0, 0, 0, time.UTC) // 05:00 in São Paulo
	planned, note, ok := c.Restrict(leg, at)
	if !ok || !planned.Equal(time.Date(2026, 10, 27, 1, 30, 0, 0, time.UTC)) {
		t.Fatalf("05:00: %v %v", planned, ok)
	}
	if want := "It would reach the São Paulo truck restriction at 05:00 local time; by standing policy it waits at Guarulhos staging yard until 21:00 and delivers about 22:30."; note != want {
		t.Errorf("note:\n got %s\nwant %s", note, want)
	}
	for name, c2 := range map[string]struct {
		b  store.Booking
		at time.Time
	}{
		"04:59":         {leg, at.Add(-time.Minute)},
		"21:00":         {leg, time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)},
		"another city":  {store.Booking{Road: &store.RoadBooking{Request: intents.RoadBookingRequestBody{Delivery: intents.Stop{UNLocationCode: "BRCPQ"}}}}, at},
		"an ocean leg":  {store.Booking{}, at},
		"no curfew set": {leg, at},
	} {
		cf := c
		if name == "no curfew set" {
			cf = nil
		}
		if got, note, ok := cf.Restrict(c2.b, c2.at); ok || !got.Equal(c2.at) || note != "" {
			t.Errorf("%s: %v %q %v", name, got, note, ok)
		}
	}
	if _, _, ok := c.Restrict(leg, time.Date(2026, 10, 26, 23, 59, 0, 0, time.UTC)); !ok {
		t.Error("20:59: not restricted")
	}
}

// TestTruckCurfew: a delay that brings a truck into the restriction is
// re-planned by the standing policy: the alert's new estimate is the
// policy's, its summary says why, and the leg is told.
func TestTruckCurfew(t *testing.T) {
	s := newTruckSim(t)
	var told []string
	s.sim.Restrict = saoPaulo(t).Restrict
	s.sim.OnRestricted = func(b store.Booking, note string, until time.Time) {
		told = append(told, b.Ref+" "+until.Format(time.RFC3339))
	}
	for range 5 {
		s.advance(time.Second)
	}
	// Planned 2026-10-25 17:00 in São Paulo; 12h late is 05:00.
	alert, err := s.sim.Disrupt("SR-BKG-0913", 12*time.Hour, "Heavy rain on BR-116.")
	if err != nil {
		t.Fatal(err)
	}
	if alert.NewETA != "2026-10-27T01:30:00Z" || alert.Severity != intents.SeverityMedium ||
		!strings.HasPrefix(alert.Summary, "Heavy rain on BR-116. It would reach") || len(told) != 1 || told[0] != "SR-BKG-0913 2026-10-27T01:30:00Z" {
		t.Fatalf("alert %+v, told %v", alert, told)
	}
	for range 60 {
		s.advance(time.Second)
	}
	if arr := s.pub.list()[5].(*intents.TnTEventBody); arr.EventDateTime != "2026-10-27T01:30:00Z" {
		t.Errorf("arrival %s", arr.EventDateTime)
	}
	// Outside the window: as before.
	s2 := newTruckSim(t)
	s2.sim.Restrict = saoPaulo(t).Restrict
	if a, err := s2.sim.Disrupt("SR-BKG-0913", 6*time.Hour, "Road closed near Jundiaí."); err != nil || a.NewETA != "2026-10-26T02:00:00Z" || a.Summary != "Road closed near Jundiaí." {
		t.Errorf("23:00: %+v %v", a, err)
	}
}

// TestTruckCurfewLongReason: a reason too long to share the summary with
// the policy's note gives way to it; staging can't take a leg past the
// longest delay.
func TestTruckCurfewLongReason(t *testing.T) {
	s := newTruckSim(t)
	s.sim.Restrict = saoPaulo(t).Restrict
	alert, err := s.sim.Disrupt("SR-BKG-0913", 12*time.Hour, strings.Repeat("rain ", 50))
	if err != nil || !strings.HasPrefix(alert.Summary, "It would reach") || len([]rune(alert.Summary)) > 280 {
		t.Fatalf("%+v %v", alert, err)
	}
	s2 := newTruckSim(t)
	s2.sim.Restrict = func(b store.Booking, at time.Time) (time.Time, string, bool) {
		return at.Add(maxDisruptDelay), "Staged for a month.", true
	}
	if _, err := s2.sim.Disrupt("SR-BKG-0913", time.Hour, "Rain."); err == nil {
		t.Error("staged past the longest delay")
	}
	// A reason refused without a curfew is refused with one: none, or
	// one too long.
	for _, reason := range []string{"", strings.Repeat("r", 281)} {
		if a, err := s.sim.Disrupt("SR-BKG-0913", 12*time.Hour, reason); err == nil {
			t.Errorf("reason of %d: %+v", len(reason), a)
		}
	}
}

// TestTruckCurfewPublishFails: the policy's step is told only of a
// re-plan that happened: not when the estimate fails (nothing moved), and
// once when only the alert fails (the delay stands). Nor is it told of a
// delay the curfew doesn't touch.
func TestTruckCurfewPublishFails(t *testing.T) {
	s := newTruckSim(t)
	told := 0
	s.sim.Restrict = saoPaulo(t).Restrict
	s.sim.OnRestricted = func(store.Booking, string, time.Time) { told++ }
	s.pub.fail = errors.New("down")
	if _, err := s.sim.Disrupt("SR-BKG-0913", 12*time.Hour, "Heavy rain."); err == nil || told != 0 {
		t.Fatalf("estimate failed: %v, told %d", err, told)
	}
	s.pub.fail = nil
	// Nothing moved: 6h late is still 23:00, outside the window.
	if a, err := s.sim.Disrupt("SR-BKG-0913", 6*time.Hour, "Road closed."); err != nil || a.NewETA != "2026-10-26T02:00:00Z" || told != 0 {
		t.Fatalf("after: %+v %v, told %d", a, err, told)
	}
	s.pub.failAlerts = errors.New("down")
	// Now 23:00; 7h more is 06:00, inside.
	if _, err := s.sim.Disrupt("SR-BKG-0913", 7*time.Hour, "More rain."); !errors.Is(err, ErrAlertNotSent) || told != 1 {
		t.Fatalf("alert failed: %v, told %d", err, told)
	}
}
