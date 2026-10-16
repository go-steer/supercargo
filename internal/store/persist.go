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
	"fmt"
	"slices"
	"time"
)

// Saver keeps a store's entries across restarts: a bucket of
// internal/persist. A store given one loads what it holds, then writes
// each change through to it before the change takes effect; a change the
// saver refuses doesn't happen.
type Saver interface {
	Put(key string, v any) error
	// DeleteBefore removes every key ordered before key.
	DeleteBefore(key string) error
	Load(fn func(key string, raw []byte) error) error
}

func (k bookingKey) String() string { return k.operator + " " + k.ref }

// Persist loads the bookings sv holds into s, then saves every booking
// added or changed. Call it once, before s is used.
func (s *Bookings) Persist(sv Saver) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := sv.Load(func(_ string, raw []byte) error {
		var b Booking
		if err := json.Unmarshal(raw, &b); err != nil {
			return err
		}
		s.m[bookingKey{b.Operator, b.Ref}] = b
		return nil
	}); err != nil {
		return err
	}
	s.sv = sv
	return nil
}

// savedRecord is one record as saved: its booking, and its body as the
// exact bytes it arrived with (a JSON value would be re-encoded: spaces
// dropped, <, > and & escaped), apart from the rest of it.
type savedRecord struct {
	Operator, Ref string
	Record        Record
	Body          []byte
}

func saveRecord(k bookingKey, r Record) savedRecord {
	body := r.Body
	r.Body = nil
	return savedRecord{k.operator, k.ref, r, body}
}

// Persist loads the records sv holds into l, in the order they were
// added, then saves every record added. Call it once, before l is used.
func (l *EventLog) Persist(sv Saver) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := sv.Load(func(_ string, raw []byte) error {
		var s savedRecord
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
		k := bookingKey{s.Operator, s.Ref}
		s.Record.Body = s.Body
		if l.ids[k] == nil {
			l.ids[k] = map[string]bool{}
		}
		l.ids[k][s.Record.Intent+" "+s.Record.ID] = true
		l.m[k] = append(l.m[k], s.Record)
		l.seq++
		return nil
	}); err != nil {
		return err
	}
	l.sv = sv
	return nil
}

// recordKey orders a booking's records by when they were added (the log's
// sequence), as the saver loads keys in order.
func recordKey(k bookingKey, seq uint64) string { return fmt.Sprintf("%020d %s", seq, k) }

// Persist loads the entries sv holds into r, dropping those past the
// retention, then saves every entry recorded and forgets every entry
// expired. Call it once, before r is used.
func (r *Replay) Persist(sv Saver) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.opts.Now()
	var loaded []queued
	if err := sv.Load(func(_ string, raw []byte) error {
		var s struct {
			Sender, MessageID string
			At                time.Time
		}
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
		if now.Sub(s.At) >= r.opts.Retention {
			return nil // forgotten below
		}
		loaded = append(loaded, queued{replayKey{s.Sender, s.MessageID}, s.At})
		return nil
	}); err != nil {
		return err
	}
	if err := sv.DeleteBefore(replayCutoff(now, r.opts.Retention)); err != nil {
		return err
	}
	slices.SortStableFunc(loaded, func(a, b queued) int { return a.at.Compare(b.at) })
	for _, q := range loaded {
		r.seen[q.k] = q.at
		r.count[q.k.sender]++
		r.queue = append(r.queue, q)
	}
	r.sv = sv
	return nil
}

// replayEntryKey orders entries by when they were recorded, so those past
// the retention go in one range (replayCutoff).
func replayEntryKey(k replayKey, at time.Time) string {
	return fmt.Sprintf("%020d %s %s", at.UnixNano(), k.sender, k.messageID)
}

// replayCutoff is the first key of an entry still within the retention at
// now: every key before it is one recorded at least retention ago.
func replayCutoff(now time.Time, retention time.Duration) string {
	return fmt.Sprintf("%020d", now.Add(-retention).UnixNano()+1)
}
