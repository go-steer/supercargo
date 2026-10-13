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
	"context"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/store"
)

// TestSharedClockReanchors: once nothing is playing, the next booking
// anchors the shared clock afresh, so a journey booked later plays from
// its own time, not in a burst.
func TestSharedClockReanchors(t *testing.T) {
	now := roadNow
	sim := &Simulator{Publisher: &published{}, HoursPerSecond: 1, StartDelay: time.Second, Shared: true, Now: func() time.Time { return now }}
	s, _, _ := roadServices()
	s.OnBooked = func(b store.Booking) {
		if err := sim.Start(b); err != nil {
			t.Fatal(err)
		}
	}
	first := roadBody()
	first.Pickup.Earliest = ""
	if _, err := s.Handle(context.Background(), roadRequest(meridian, first)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute) // an hour of simulated time per second: the leg is long done
	sim.Advance()
	if sim.Active() != 0 || !sim.anchor.IsZero() {
		t.Fatalf("after the leg: %d active, anchor %v", sim.Active(), sim.anchor)
	}
	now = now.Add(time.Hour) // much later, a second journey
	tl := sim.newTimeline(store.Booking{}, now, now)
	if !tl.realStart.Equal(now) || !tl.simStart.Equal(now) {
		t.Errorf("anchored at %v / %v, want now (%v)", tl.realStart, tl.simStart, now)
	}
}
