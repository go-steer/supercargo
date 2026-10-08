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
	"strings"
	"sync"
	"time"

	"github.com/go-steer/supercargo/pkg/envelope"
)

// Message is one envelope this node sent or received, as it was on the
// wire, with what the node decided about it: the payload view's record
// (docs/DESIGN.md, A.12). A node only ever holds its own side of a
// conversation.
type Message struct {
	Seq  uint64    `json:"seq"`
	Time time.Time `json:"time"`
	// Side is the role the message belongs to ("operator" or "customer"),
	// or empty when no role can be told (a message that didn't parse, or
	// an intent the node doesn't serve): those belong to the whole node.
	Side      string `json:"side,omitempty"`
	Direction string `json:"dir"` // "in" or "out"
	Via       string `json:"via,omitempty"`
	Intent    string `json:"intent,omitempty"`
	Sender    string `json:"sender,omitempty"`
	Receiver  string `json:"receiver,omitempty"`
	MessageID string `json:"messageId,omitempty"`
	KID       string `json:"kid,omitempty"`
	// Verified is whether the signature verified. A message that didn't
	// carries its header as claimed and no body: show it as text, never as
	// anything the sender proved.
	Verified bool   `json:"verified"`
	Outcome  string `json:"outcome"`
	Code     string `json:"code,omitempty"`
	Reason   string `json:"reason,omitempty"`
	// Conversation is the header's correlationId: the messages of one
	// exchange share it.
	Conversation string           `json:"conversation,omitempty"`
	Header       *envelope.Header `json:"header,omitempty"`
	Body         json.RawMessage  `json:"body,omitempty"`
	// Wire is the exact bytes, as text (invalid UTF-8 replaced), at most
	// envelope.MaxSize of them, or MaxUnverifiedWire for a message whose
	// signature didn't verify (Truncated says it was cut).
	Wire      string `json:"wire"`
	Truncated bool   `json:"truncated,omitempty"`
}

// size is what a message costs the log's byte budget.
func (m *Message) size() int { return len(m.Wire) + len(m.Body) + len(m.Reason) + 512 }

// unverified is a message nobody proved they sent: anyone can post those,
// so they have a quota of their own and can't push out the rest.
func (m *Message) unverified() bool { return m.Direction == "in" && !m.Verified }

// MaxUnverifiedWire is the most wire text kept for an unverified message.
const MaxUnverifiedWire = 4 << 10

// copyMessage is m with nothing shared: the body and the header's
// pointers are copied.
func copyMessage(m Message) Message {
	m.Body = slices.Clone(m.Body)
	if m.Header != nil {
		h := *m.Header
		h.InReplyTo, h.BookingRef = cloneString(h.InReplyTo), cloneString(h.BookingRef)
		if h.Dialect != nil {
			d := *h.Dialect
			h.Dialect = &d
		}
		m.Header = &h
	}
	return m
}

// Messages is the node's message log: the last messages it sent and
// received, bounded by count and by bytes, oldest dropped first.
type Messages struct {
	keep, budget int

	mu         sync.Mutex
	seq        uint64
	bytes      int
	unverified int       // how many of list are unverified
	list       []Message // oldest first
}

// NewMessages keeps at most keep messages (1,000 if keep <= 0) and budget
// bytes of them (16 MiB if budget <= 0).
func NewMessages(keep, budget int) *Messages {
	if keep <= 0 {
		keep = 1000
	}
	if budget <= 0 {
		budget = 16 << 20
	}
	return &Messages{keep: keep, budget: budget}
}

// Add records m, assigning its sequence number, and returns a copy. Wire
// is made valid UTF-8 and cut to its limit; a body that isn't valid JSON
// is dropped (it would break every reader). Unverified messages may fill
// at most a fifth of the log, by count and by bytes: past that, the oldest
// unverified one goes, so a flood of junk can't wipe out the record of
// what partners actually sent.
func (l *Messages) Add(m Message) Message {
	limit := envelope.MaxSize
	if m.unverified() {
		m.Body = nil // unverified bytes are shown as wire text only
		limit = MaxUnverifiedWire
	}
	m.Wire = strings.ToValidUTF8(m.Wire, "�")
	if len(m.Wire) > limit {
		m.Wire, m.Truncated = strings.ToValidUTF8(m.Wire[:limit], ""), true
	}
	if m.Body != nil && !json.Valid(m.Body) {
		m.Body = nil
	}
	m = copyMessage(m)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	m.Seq = l.seq
	l.list = append(l.list, m)
	l.bytes += m.size()
	if m.unverified() {
		l.unverified++
		for l.unverified > max(1, l.keep/5) || l.unverifiedBytes() > l.budget/5 {
			l.drop(slices.IndexFunc(l.list, func(m Message) bool { return m.unverified() }))
		}
	}
	for len(l.list) > 1 && (len(l.list) > l.keep || l.bytes > l.budget) {
		l.drop(0)
	}
	return copyMessage(m)
}

// drop removes message i.
func (l *Messages) drop(i int) {
	l.bytes -= l.list[i].size()
	if l.list[i].unverified() {
		l.unverified--
	}
	l.list = slices.Delete(l.list, i, i+1)
}

func (l *Messages) unverifiedBytes() int {
	n := 0
	for i := range l.list {
		if l.list[i].unverified() {
			n += l.list[i].size()
		}
	}
	return n
}

// MessageFilter selects messages. Zero values select everything.
type MessageFilter struct {
	// Sides keeps messages of these sides, plus those with no side (the
	// whole node's). Empty keeps every side.
	Sides []string
	// Since keeps messages after this sequence number.
	Since uint64
	// Conversation keeps one conversation's messages.
	Conversation string
	// Limit keeps the newest Limit of what's left (all if <= 0).
	Limit int
}

func (f MessageFilter) match(m *Message) bool {
	return m.Seq > f.Since &&
		(len(f.Sides) == 0 || m.Side == "" || slices.Contains(f.Sides, m.Side)) &&
		(f.Conversation == "" || m.Conversation == f.Conversation)
}

// List returns the messages f selects, oldest first, sharing nothing with
// the log.
func (l *Messages) List(f MessageFilter) []Message {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := []Message{}
	for i := range l.list {
		if f.match(&l.list[i]) {
			out = append(out, copyMessage(l.list[i]))
		}
	}
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[len(out)-f.Limit:]
	}
	return out
}

// Get returns message seq if it is still kept and f's sides allow it.
func (l *Messages) Get(seq uint64, sides []string) (Message, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := MessageFilter{Sides: sides}
	for i := range l.list {
		if l.list[i].Seq == seq {
			if !f.match(&l.list[i]) {
				return Message{}, false
			}
			return copyMessage(l.list[i]), true
		}
	}
	return Message{}, false
}

func cloneString(s *string) *string {
	if s == nil {
		return nil
	}
	v := *s
	return &v
}
