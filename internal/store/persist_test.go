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
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/supercargo/pkg/intents"
)

// memSaver is a Saver in a map; fail makes Put and Delete fail, and
// failLoad Load.
type memSaver struct {
	m              map[string][]byte
	fail, failLoad bool
}

var errSave = errors.New("disk full")

func newMem() *memSaver { return &memSaver{m: map[string][]byte{}} }

func (s *memSaver) Put(key string, v any) error {
	if s.fail {
		return errSave
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.m[key] = raw
	return nil
}

func (s *memSaver) DeleteBefore(key string) error {
	if s.fail {
		return errSave
	}
	for k := range s.m {
		if k < key {
			delete(s.m, k)
		}
	}
	return nil
}

// saved reports whether some key ends with suffix.
func (s *memSaver) saved(suffix string) bool {
	for k := range s.m {
		if strings.HasSuffix(k, suffix) {
			return true
		}
	}
	return false
}

func (s *memSaver) Load(fn func(string, []byte) error) error {
	if s.failLoad {
		return errSave
	}
	for _, k := range slices.Sorted(maps.Keys(s.m)) {
		if err := fn(k, s.m[k]); err != nil {
			return err
		}
	}
	return nil
}

// TestBookingsPersist: bookings added and changed are there after a
// restart; a change the saver refuses doesn't happen.
func TestBookingsPersist(t *testing.T) {
	sv := newMem()
	s := NewBookings()
	if err := s.Persist(sv); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	_ = s.Add(Booking{Ref: "OB-1", Operator: "ob.example", Shipper: "cy.example", CreatedAt: at,
		Road: &RoadBooking{Confirmation: intents.RoadBookingConfirmationBody{BookingRef: "OB-1", Status: intents.StatusConfirmed}}})
	_, _ = s.Update("ob.example", "OB-1", func(b *Booking) error { b.CancelledAt = at; return nil })
	again := NewBookings()
	if err := again.Persist(sv); err != nil {
		t.Fatal(err)
	}
	b, err := again.Get("ob.example", "OB-1")
	if err != nil || !b.Cancelled() || !b.CreatedAt.Equal(at) || b.Road.Confirmation.Status != intents.StatusConfirmed || b.Shipper != "cy.example" {
		t.Fatalf("restored %+v, %v", b, err)
	}
	sv.fail = true
	if err := again.Add(Booking{Ref: "OB-2", Operator: "ob.example"}); !errors.Is(err, errSave) || again.Len() != 1 {
		t.Errorf("add refused by the saver: %v, %d held", err, again.Len())
	}
	if _, err := again.Update("ob.example", "OB-1", func(b *Booking) error { b.Shipper = "x"; return nil }); !errors.Is(err, errSave) {
		t.Errorf("update refused by the saver: %v", err)
	}
	if b, _ := again.Get("ob.example", "OB-1"); b.Shipper != "cy.example" {
		t.Error("a refused update took effect")
	}
	sv.failLoad = true
	if err := NewBookings().Persist(sv); err == nil {
		t.Error("loaded from a failing saver")
	}
	sv.failLoad, sv.m["x"] = false, []byte("not json")
	if err := NewBookings().Persist(sv); err == nil {
		t.Error("loaded a booking that isn't JSON")
	}
}

// TestEventLogPersist: records come back in the order they were added,
// still once per ID, and new ones follow them; a record the saver refuses
// isn't added.
func TestEventLogPersist(t *testing.T) {
	sv := newMem()
	l := NewEventLog(0)
	if err := l.Persist(sv); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"EV-2", "EV-10", "EV-1"} {
		_, _ = l.Add("ob.example", "OB-1", Record{ID: id, Intent: intents.TnTEvent, Body: json.RawMessage(`{}`)})
	}
	_, _ = l.Add("ob.example", "OB-2", Record{ID: "EV-1", Intent: intents.TnTEvent, Body: json.RawMessage(`{}`)})
	again := NewEventLog(0)
	if err := again.Persist(sv); err != nil {
		t.Fatal(err)
	}
	ids := func(ref string) []string {
		var out []string
		for _, r := range again.List("ob.example", ref) {
			out = append(out, r.ID)
		}
		return out
	}
	if got := ids("OB-1"); !slices.Equal(got, []string{"EV-2", "EV-10", "EV-1"}) {
		t.Errorf("restored %v", got)
	}
	if added, err := again.Add("ob.example", "OB-1", Record{ID: "EV-10", Intent: intents.TnTEvent}); added || err != nil {
		t.Errorf("a restored ID added again: %v, %v", added, err)
	}
	if added, err := again.Add("ob.example", "OB-1", Record{ID: "EV-3", Intent: intents.TnTEvent}); !added || err != nil {
		t.Fatalf("a new record: %v, %v", added, err)
	}
	third := NewEventLog(0)
	_ = third.Persist(sv)
	if got := third.List("ob.example", "OB-1"); len(got) != 4 || got[3].ID != "EV-3" || len(third.List("ob.example", "OB-2")) != 1 {
		t.Errorf("after a second restart: %v", got)
	}
	sv.fail = true
	if added, err := third.Add("ob.example", "OB-1", Record{ID: "EV-4", Intent: intents.TnTEvent}); added || !errors.Is(err, errSave) {
		t.Errorf("refused by the saver: %v, %v", added, err)
	}
	sv.fail = false
	if added, _ := third.Add("ob.example", "OB-1", Record{ID: "EV-4", Intent: intents.TnTEvent}); !added {
		t.Error("a record the saver refused before is now a duplicate")
	}
	sv.m["x"] = []byte("not json")
	if err := NewEventLog(0).Persist(sv); err == nil {
		t.Error("loaded a record that isn't JSON")
	}
}

// TestEventLogPersistBytes: a record's body comes back as the exact bytes
// it was added with: spacing kept, nothing escaped.
func TestEventLogPersistBytes(t *testing.T) {
	sv := newMem()
	l := NewEventLog(0)
	_ = l.Persist(sv)
	body := json.RawMessage("{ \"description\" : \"Rotterdam <ECT> & Delta\",\n  \"n\": 1.50 }")
	_, _ = l.Add("ob.example", "OB-1", Record{ID: "EV-1", Intent: intents.TnTEvent, Body: body})
	again := NewEventLog(0)
	if err := again.Persist(sv); err != nil {
		t.Fatal(err)
	}
	if got := again.List("ob.example", "OB-1")[0].Body; string(got) != string(body) {
		t.Errorf("restored body %q, want %q", got, body)
	}
}

// TestReplayPersist: a message seen before a restart is still a replay
// after it; entries past the retention are dropped and forgotten; an
// entry the saver refuses refuses the message.
func TestReplayPersist(t *testing.T) {
	now := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	sv := newMem()
	r := NewReplay(ReplayOptions{Retention: 10 * time.Minute, Now: clock})
	if err := r.Persist(sv); err != nil {
		t.Fatal(err)
	}
	_ = r.Record("cy.example", "m-old")
	now = now.Add(6 * time.Minute)
	_ = r.Record("cy.example", "m-new")
	_ = r.Record("ob.example", "m-new")

	now = now.Add(5 * time.Minute) // m-old is 11 minutes old, m-new 5
	again := NewReplay(ReplayOptions{Retention: 10 * time.Minute, Now: clock})
	if err := again.Persist(sv); err != nil {
		t.Fatal(err)
	}
	if err := again.Record("cy.example", "m-new"); !errors.Is(err, ErrReplay) {
		t.Errorf("replayed after a restart: %v", err)
	}
	if again.Len() != 2 || len(sv.m) != 2 {
		t.Errorf("held %d, saved %d: the stale entry stayed", again.Len(), len(sv.m))
	}
	now = now.Add(6 * time.Minute) // m-new expires as the next is recorded
	if err := again.Record("cy.example", "m-3"); err != nil {
		t.Fatal(err)
	}
	if sv.saved(" cy.example m-new") || !sv.saved(" cy.example m-3") {
		t.Error("an expired entry is still saved, or a live one isn't")
	}
	sv.fail = true
	if err := again.Record("cy.example", "m-4"); !errors.Is(err, errSave) {
		t.Errorf("refused by the saver: %v", err)
	}
	sv.fail = false
	if err := again.Record("cy.example", "m-4"); err != nil {
		t.Errorf("a message the saver refused before is now a replay: %v", err)
	}
	now = now.Add(time.Hour)
	sv.fail = true
	if err := again.Record("cy.example", "m-5"); !errors.Is(err, errSave) || again.Len() != 2 {
		t.Errorf("expiry the saver refused: %v, held %d", err, again.Len())
	}
	sv.fail, sv.failLoad = false, true
	if err := NewReplay(ReplayOptions{}).Persist(sv); err == nil {
		t.Error("loaded from a failing saver")
	}
	sv.failLoad, sv.m["x"] = false, []byte("not json")
	if err := NewReplay(ReplayOptions{}).Persist(sv); err == nil {
		t.Error("loaded an entry that isn't JSON")
	}
	delete(sv.m, "x")
	sv.fail = true
	if err := NewReplay(ReplayOptions{Retention: time.Minute, Now: func() time.Time { return now.Add(time.Hour) }}).Persist(sv); err == nil {
		t.Error("dropped stale entries the saver couldn't forget")
	}
}

// TestEventLogIDs: the IDs of every record of every booking.
func TestEventLogIDs(t *testing.T) {
	l := NewEventLog(0)
	_, _ = l.Add("ob.example", "OB-1", Record{ID: "EV-1", Intent: intents.TnTEvent})
	_, _ = l.Add("ob.example", "OB-2", Record{ID: "AL-2", Intent: intents.TnTAlert})
	if got := slices.Sorted(slices.Values(l.IDs())); !slices.Equal(got, []string{"AL-2", "EV-1"}) {
		t.Errorf("IDs %v", got)
	}
}
