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

package mockcarrier

import (
	"testing"
	"time"
)

// TestEDIFACTSharedClock: on the shared clock, a trip's statuses come at
// their real times counted from the first instruction, not an hour before
// each pickup: a pickup 10 hours after the instruction waits 10 seconds
// at 1 h/s.
func TestEDIFACTSharedClock(t *testing.T) {
	r := newRig(t)
	r.polder.Shared = true
	r.now = time.Date(2026, 10, 21, 7, 0, 0, 0, time.UTC).Add(-10 * time.Hour) // the pickup is 10 h away
	if _, err := r.toPol.Put("MF000031", instruction("meridian-forwarding.example")); err != nil {
		t.Fatal(err)
	}
	if err := r.polder.Step(); err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(5 * time.Second)
	if err := r.polder.Step(); err != nil {
		t.Fatal(err)
	}
	if _, sts := r.replies(t); len(sts) != 0 {
		t.Fatalf("after 5 s: %d statuses, want none yet", len(sts))
	}
	r.now = r.now.Add(6 * time.Second)
	if err := r.polder.Step(); err != nil {
		t.Fatal(err)
	}
	if _, sts := r.replies(t); len(sts) == 0 {
		t.Fatal("after 11 s: no pick-up")
	}
}
