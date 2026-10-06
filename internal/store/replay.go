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

// Package store holds a node's state in memory: the message IDs it has
// seen, and the bookings it knows about. A demo node keeps everything in
// process; persistence is out of scope (docs/DESIGN.md, section A.15).
package store

import (
	"errors"
	"sync"
	"time"
)

var (
	// ErrReplay reports a message ID this sender has already used.
	ErrReplay = errors.New("store: message ID already seen")

	// ErrFull reports a store at capacity. For the replay store (for
	// everyone or for this sender), the message is refused (fail closed),
	// not admitted unrecorded.
	ErrFull = errors.New("store: full")
)

// ReplayOptions configures a Replay. Zero or negative values mean the
// defaults.
type ReplayOptions struct {
	// Retention is how long an entry is kept (default 15m). It must exceed
	// twice the node's allowed clock skew, because a message older than the
	// skew is refused as stale anyway; nothing is gained by keeping entries
	// much longer.
	Retention time.Duration
	// Max bounds the entries held across all senders (default 1,000,000).
	Max int
	// PerSender bounds one sender's entries (default 100,000), so a single
	// verified sender (and anyone with a profile is one) can exhaust only
	// its own allowance, not every partner's.
	PerSender int
	// Now replaces time.Now (tests).
	Now func() time.Time
}

// Replay remembers (sender, messageId) pairs from verified envelopes, so a
// captured message can't be delivered twice. Entries expire in the order
// they were recorded, so pruning costs only what expires: no scan of the
// whole store under its lock.
type Replay struct {
	opts ReplayOptions

	mu    sync.Mutex
	seen  map[replayKey]time.Time // → when it was recorded
	count map[string]int          // entries per sender
	queue []queued                // in recording order
	head  int                     // first live element of queue
}

type replayKey struct{ sender, messageID string }

type queued struct {
	k  replayKey
	at time.Time
}

// NewReplay returns an empty Replay.
func NewReplay(opts ReplayOptions) *Replay {
	if opts.Retention <= 0 {
		opts.Retention = 15 * time.Minute
	}
	if opts.Max <= 0 {
		opts.Max = 1_000_000
	}
	if opts.PerSender <= 0 {
		opts.PerSender = 100_000
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Replay{opts: opts, seen: map[replayKey]time.Time{}, count: map[string]int{}}
}

// Retention is how long entries are kept.
func (r *Replay) Retention() time.Duration { return r.opts.Retention }

// Record notes (sender, messageID). It returns ErrReplay if the pair was
// noted within the retention, and ErrFull if the store, or this sender's
// share of it, has no room.
func (r *Replay) Record(sender, messageID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.opts.Now()
	r.expireLocked(now)
	k := replayKey{sender, messageID}
	if _, ok := r.seen[k]; ok {
		return ErrReplay
	}
	if r.count[sender] >= r.opts.PerSender || len(r.seen) >= r.opts.Max {
		return ErrFull
	}
	r.seen[k] = now
	r.count[sender]++
	r.queue = append(r.queue, queued{k, now})
	return nil
}

// expireLocked drops entries recorded at least Retention ago, oldest first.
// If the clock steps back, the queue is no longer in order and entries are
// kept longer than needed, which is the safe direction.
func (r *Replay) expireLocked(now time.Time) {
	for r.head < len(r.queue) && now.Sub(r.queue[r.head].at) >= r.opts.Retention {
		q := r.queue[r.head]
		r.queue[r.head] = queued{}
		r.head++
		if at, ok := r.seen[q.k]; ok && at.Equal(q.at) {
			delete(r.seen, q.k)
			if r.count[q.k.sender]--; r.count[q.k.sender] <= 0 {
				delete(r.count, q.k.sender)
			}
		}
	}
	// Reclaim the consumed front once it's most of the slice.
	if r.head > 1024 && r.head > len(r.queue)/2 {
		r.queue = append([]queued(nil), r.queue[r.head:]...)
		r.head = 0
	}
}

// Len is the number of entries held.
func (r *Replay) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.seen)
}
