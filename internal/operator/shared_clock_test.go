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
	"github.com/go-steer/supercargo/pkg/intents"
)

// TestSharedClock: on the shared clock, legs play in the order of their
// real dates, from the first booking's time: a truck collecting a day
// later picks up a day of simulated time after the first leg's start, not
// at once; and nothing fires before the start delay.
func TestSharedClock(t *testing.T) {
	now := roadNow // 2026-10-20 09:30
	pub := &published{}
	sim := &Simulator{Publisher: pub, HoursPerSecond: 1, StartDelay: time.Second, Shared: true, Now: func() time.Time { return now }}
	soon, later := roadBody(), roadBody()
	soon.Pickup.Earliest, later.Pickup.Earliest = "", "2026-10-21T12:00:00Z"
	s, _, _ := roadServices()
	s.OnBooked = func(b store.Booking) {
		if err := sim.Start(b); err != nil {
			t.Fatal(err)
		}
	}
	for _, b := range []*intents.RoadBookingRequestBody{soon, later} {
		if _, err := s.Handle(context.Background(), roadRequest(meridian, b)); err != nil {
			t.Fatal(err)
		}
	}
	picked := func() (n int) {
		for _, b := range pub.list() {
			if e, ok := b.(*intents.TnTEventBody); ok && e.EventTypeCode == "PICK" {
				n++
			}
		}
		return n
	}
	// The first leg collects at 12:00 (the lead, on the hour): 2.5 h of
	// simulated time from 09:30, 2.5 s at 1 h/s.
	now = now.Add(500 * time.Millisecond)
	sim.Advance()
	if len(pub.list()) != 0 {
		t.Fatal("fired within the start delay")
	}
	now = roadNow.Add(3 * time.Second)
	sim.Advance()
	if picked() != 1 {
		t.Fatalf("after 3 s: %d pick-ups, want the first leg's", picked())
	}
	// The second collects a day later: 26.5 h from the anchor.
	now = roadNow.Add(20 * time.Second)
	sim.Advance()
	if picked() != 1 {
		t.Fatalf("after 20 s: %d pick-ups, want still 1", picked())
	}
	now = roadNow.Add(27 * time.Second)
	sim.Advance()
	if picked() != 2 {
		t.Fatalf("after 27 s: %d pick-ups, want 2", picked())
	}
}
