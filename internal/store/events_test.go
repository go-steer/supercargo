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

package store

import (
	"errors"
	"testing"
)

func TestEventLog(t *testing.T) {
	l := NewEventLog(2)
	if added, err := l.Add("ob.example", "OB-1", Record{ID: "EV-1", Body: []byte(`{"a":1}`)}); !added || err != nil {
		t.Fatalf("Add = %v, %v", added, err)
	}
	if added, err := l.Add("ob.example", "OB-1", Record{ID: "EV-1"}); added || err != nil {
		t.Errorf("duplicate Add = %v, %v; want false, nil", added, err)
	}
	// Another operator's booking with the same reference is separate.
	if added, _ := l.Add("other.example", "OB-1", Record{ID: "EV-1"}); !added {
		t.Error("same ref under another operator wasn't added")
	}
	_, _ = l.Add("ob.example", "OB-1", Record{ID: "EV-2"})
	if _, err := l.Add("ob.example", "OB-1", Record{ID: "EV-3"}); !errors.Is(err, ErrFull) {
		t.Errorf("Add past the cap = %v, want ErrFull", err)
	}
	recs := l.List("ob.example", "OB-1")
	if len(recs) != 2 || recs[0].ID != "EV-1" || recs[1].ID != "EV-2" {
		t.Errorf("List = %+v", recs)
	}
	recs[0].Body[2] = 'X'
	if l.List("ob.example", "OB-1")[0].Body[2] != 'a' {
		t.Error("List shares bodies with the log")
	}
	if len(l.List("ob.example", "missing")) != 0 {
		t.Error("List of an unknown booking isn't empty")
	}
	if NewEventLog(0).perBooking != 10_000 {
		t.Error("default cap")
	}
}

// TestEventLogKeys: an event and an alert may share an ID, and Add keeps
// its own copy of the body.
func TestEventLogKeys(t *testing.T) {
	l := NewEventLog(0)
	body := []byte(`{"a":1}`)
	if added, _ := l.Add("ob.example", "OB-1", Record{ID: "X-1", Intent: "ulp.tnt.event", Body: body}); !added {
		t.Fatal("event not added")
	}
	if added, _ := l.Add("ob.example", "OB-1", Record{ID: "X-1", Intent: "ulp.tnt.alert"}); !added {
		t.Error("an alert sharing an event's ID was taken for a duplicate")
	}
	body[2] = 'X'
	if l.List("ob.example", "OB-1")[0].Body[2] != 'a' {
		t.Error("Add shares the caller's body")
	}
}

func TestBookingsCap(t *testing.T) {
	s := NewBookings()
	s.max = 1
	_ = s.Add(Booking{Ref: "A", Operator: "o.example"})
	if err := s.Add(Booking{Ref: "B", Operator: "o.example"}); !errors.Is(err, ErrFull) {
		t.Errorf("Add past the cap = %v, want ErrFull", err)
	}
}
