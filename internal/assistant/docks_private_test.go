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
	"slices"
	"testing"
	"time"
)

// TestDockPrivate: what the model learns of an appointment, read or
// moved (its day, window and ID), is as private as the business file's
// slot: a question naming it is refused, and an order can't deliver by it.
func TestDockPrivate(t *testing.T) {
	a, d := dockAssistant(t)
	slots, _ := d.m.Slots(context.Background(), thursday.AddDate(0, 0, 10), 1)
	s := slots[0]
	out, err := a.tool(t, ToolDraftDock, `{"shipperRef":"CY-PO-55120","dock":"`+s.Dock+`","start":"`+s.Start.Format(time.RFC3339)+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Send(context.Background(), out.(DraftResult).DraftID); err != nil {
		t.Fatal(err)
	}
	day := s.Start.Format(time.DateOnly)
	window := s.Start.Format("15:04") + "-" + s.End.Format("15:04")
	for _, q := range []string{"Can you deliver on " + day + "?", "We receive " + window + " only.", "Our appointment AG-004100 moved."} {
		if a.privateIn(q) == "" {
			t.Errorf("%q passed", q)
		}
	}
	if !slices.Contains(a.private(), day) {
		t.Error("an order could deliver by the moved day")
	}
	if a.privateIn("When does the ship arrive?") != "" {
		t.Error("a plain question refused")
	}
}
