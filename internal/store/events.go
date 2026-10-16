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
	"slices"
	"sync"
	"time"

	"github.com/go-steer/supercargo/pkg/envelope"
)

// Record is one event or alert about a booking, kept as the verified
// header and body bytes it arrived with (or was sent with), for audit.
type Record struct {
	ID     string // eventId or alertId
	Intent string
	Header envelope.Header
	Body   json.RawMessage
	At     time.Time // when it was recorded
}

// EventLog holds the events and alerts of each booking, in arrival order,
// keyed by (operator, reference) like Bookings. Adding is idempotent by
// (intent, ID): an event and an alert may share an ID without one hiding
// the other,
// and a push delivered twice is recorded once.
type EventLog struct {
	perBooking int

	mu  sync.Mutex
	m   map[bookingKey][]Record
	ids map[bookingKey]map[string]bool
	seq uint64 // records added, saved ones included
	sv  Saver  // nil: in memory only
}

// NewEventLog returns an empty log holding at most perBooking records per
// booking (default 10,000).
func NewEventLog(perBooking int) *EventLog {
	if perBooking <= 0 {
		perBooking = 10_000
	}
	return &EventLog{perBooking: perBooking, m: map[bookingKey][]Record{}, ids: map[bookingKey]map[string]bool{}}
}

// Add appends r to the booking's log. It reports false, with no error, for
// an ID the booking already has, and returns ErrFull when the booking's log
// is at capacity.
func (l *EventLog) Add(operator, ref string, r Record) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := bookingKey{operator, ref}
	if l.ids[k][r.Intent+" "+r.ID] {
		return false, nil
	}
	if len(l.m[k]) >= l.perBooking {
		return false, ErrFull
	}
	r.Body = slices.Clone(r.Body)
	if l.sv != nil {
		if err := l.sv.Put(recordKey(k, l.seq+1), saveRecord(k, r)); err != nil {
			return false, err
		}
	}
	l.seq++
	if l.ids[k] == nil {
		l.ids[k] = map[string]bool{}
	}
	l.ids[k][r.Intent+" "+r.ID] = true
	l.m[k] = append(l.m[k], r)
	return true, nil
}

// IDs returns the IDs of every record held, of every booking: what a
// restarted node numbers its new events and alerts after.
func (l *EventLog) IDs() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, recs := range l.m {
		for _, r := range recs {
			out = append(out, r.ID)
		}
	}
	return out
}

// List returns a copy of the booking's records, oldest first.
func (l *EventLog) List(operator, ref string) []Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := slices.Clone(l.m[bookingKey{operator, ref}])
	for i := range out {
		out[i].Body = slices.Clone(out[i].Body)
	}
	return out
}
