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

// Package activity is a node's activity stream: one structured entry for
// everything the node does that someone watching should see (each
// envelope in or out and the decision made about it, each event or alert
// recorded or published, each model call, each voyage started). The node's
// log, the control API's live stream, and later the TUI, the browser UI and
// the pretty log all read the same entries, so they can't disagree.
package activity

import (
	"slices"
	"sync"
	"time"

	"github.com/go-steer/supercargo/pkg/intents"
)

// Kinds of entries.
const (
	KindEnvelope = "envelope"
	KindRecord   = "record"
	KindModel    = "model"
	KindVoyage   = "voyage"
	KindPush     = "push"
	KindSummary  = "summary"
	KindTool     = "tool"
	KindDock     = "dock"
)

// Entry is one thing the node did. Exactly one of the kind-specific fields
// is set, matching Kind.
type Entry struct {
	Seq  uint64    `json:"seq"`
	Time time.Time `json:"time"`
	Kind string    `json:"kind"`
	// Side is the role the entry belongs to (SideOperator or SideCustomer),
	// or empty for the whole node's (a message that didn't parse).
	Side     string    `json:"side,omitempty"`
	Envelope *Envelope `json:"envelope,omitempty"`
	Record   *Record   `json:"record,omitempty"`
	Model    *Model    `json:"model,omitempty"`
	Voyage   *Voyage   `json:"voyage,omitempty"`
	Push     *Push     `json:"push,omitempty"`
	Summary  *Summary  `json:"summary,omitempty"`
	Tool     *Tool     `json:"tool,omitempty"`
	Dock     *Dock     `json:"dock,omitempty"`
}

// Dock is what a standing dock policy did for a truck (docks.Policy):
// booked, moved or kept its appointment, or couldn't (Error), the policy
// and who set it saying so.
type Dock struct {
	Booking   string    `json:"booking"`
	Action    string    `json:"action"`
	Dock      string    `json:"dock,omitempty"`
	Start     time.Time `json:"start,omitzero"`
	End       time.Time `json:"end,omitzero"`
	FromDock  string    `json:"fromDock,omitempty"`
	FromStart time.Time `json:"fromStart,omitzero"`
	Arrival   string    `json:"arrival"`
	Policy    string    `json:"policy"`
	Error     string    `json:"error,omitempty"`
}

// Tool is a tool call an outside agent made on this node (over MCP): which
// tool, and how it went. Never its arguments or its result, which may
// carry the company's data; the stream is everyone's on the side.
type Tool struct {
	Via     string `json:"via"`              // "mcp"
	Name    string `json:"name"`             // the tool's name, as declared
	Outcome string `json:"outcome"`          // ok, refused, or drafted
	Reason  string `json:"reason,omitempty"` // why refused, one capped line
	Draft   string `json:"draft,omitempty"`  // the draft it made, if any
	// Caller is who the agent acts for, when the node signed them in
	// (an email, with Google sign-in); empty with a shared token.
	Caller string `json:"caller,omitempty"`
}

// Envelope is a message in or out and what the node decided about it
// (node.Event).
type Envelope struct {
	Direction string `json:"dir"` // "in" or "out"
	Intent    string `json:"intent"`
	Sender    string `json:"sender"`
	Receiver  string `json:"receiver"`
	Via       string `json:"via"`
	MessageID string `json:"messageId"`
	KID       string `json:"kid"`
	Verified  bool   `json:"verified"`
	// Outcome is accepted, rejected or malformed (in), or sent, response or
	// failed (out).
	Outcome string `json:"outcome"`
	Code    string `json:"code,omitempty"`
	// Reason is the node's own account of a refusal. It goes to this node's
	// operator (the log, the control API), never to a partner.
	Reason string `json:"reason,omitempty"`
}

// Sides: which of a node's roles an entry belongs to. The shipper and
// receiver roles are one side, the customer's (docs/DESIGN.md, A.6).
const (
	SideOperator = "operator"
	SideCustomer = "customer"
)

// Visible reports whether e belongs to one of sides, or to the whole
// node. No sides means every side.
func (e Entry) Visible(sides []string) bool {
	return len(sides) == 0 || e.Side == "" || slices.Contains(sides, e.Side)
}

// Actions on a record.
const (
	ActionRecorded  = "recorded"  // received, verified and stored (shipper)
	ActionPublished = "published" // stored and queued for the subscriber (operator)
)

// Record is an event or alert recorded or published for a booking.
type Record struct {
	Action  string                `json:"action"`
	Booking string                `json:"booking"`
	Event   *intents.TnTEventBody `json:"event,omitempty"`
	Alert   *intents.TnTAlertBody `json:"alert,omitempty"`
}

// Model is one model call.
type Model struct {
	// Agent is which model role called: "inquiry responder", "alert
	// summarizer", or "assistant" (with whose, in the browser).
	Agent        string   `json:"agent,omitempty"`
	Booking      string   `json:"booking"`
	Attempt      int      `json:"attempt"`
	Model        string   `json:"model"`
	PromptHash   string   `json:"promptHash"`
	Skills       []string `json:"skills,omitempty"`
	InputTokens  int      `json:"inputTokens"`
	OutputTokens int      `json:"outputTokens"`
	LatencyMS    int64    `json:"latencyMs"`
	Outcome      string   `json:"outcome"` // ok, invalid or error
	Reason       string   `json:"reason,omitempty"`
	// Tools are the tools the reply asked for, by name (the assistant's).
	Tools []string `json:"tools,omitempty"`
}

// Voyage is a simulated voyage starting, or failing to.
type Voyage struct {
	Booking        string  `json:"booking"`
	Started        bool    `json:"started"`
	Departure      string  `json:"departure,omitempty"`
	Arrival        string  `json:"arrival,omitempty"`
	HoursPerSecond float64 `json:"hoursPerSecond,omitempty"`
	Error          string  `json:"error,omitempty"`
	// Cancelled marks a booking its shipper cancelled: its voyage ends.
	Cancelled bool `json:"cancelled,omitempty"`
}

// Push is a push the operator gave up on, or couldn't publish.
type Push struct {
	Booking string `json:"booking"`
	ID      string `json:"id,omitempty"`
	// Problem is "dropped" (given up on) or "unpublished" (not recorded).
	Problem string `json:"problem"`
	Error   string `json:"error"`
}

// Bus fans entries out to subscribers, keeping the most recent ones so a
// subscriber that connects (or reconnects) late can catch up. Publishing
// never blocks: a subscriber that falls behind is cut off, and catches up
// from the history when it reconnects.
type Bus struct {
	keep int
	now  func() time.Time

	mu      sync.Mutex
	seq     uint64
	history []Entry // the last keep entries, oldest first
	subs    map[*subscriber]struct{}
	sinks   []func(Entry)
	closed  bool
}

type subscriber struct {
	ch chan Entry
}

// NewBus returns a Bus keeping the last keep entries (default 1000).
func NewBus(keep int) *Bus {
	if keep <= 0 {
		keep = 1000
	}
	return &Bus{keep: keep, now: time.Now, subs: map[*subscriber]struct{}{}}
}

// Sink adds f, called synchronously with every entry in order (the log).
// Add sinks before publishing.
func (b *Bus) Sink(f func(Entry)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sinks = append(b.sinks, f)
}

// Publish stamps e with the next sequence number and the time, and hands
// it to the sinks and subscribers.
func (b *Bus) Publish(e Entry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	e.Seq = b.seq
	if e.Time.IsZero() {
		e.Time = b.now()
	}
	b.history = append(b.history, e)
	if len(b.history) > b.keep {
		b.history = b.history[len(b.history)-b.keep:]
	}
	for _, f := range b.sinks {
		f(e)
	}
	for s := range b.subs {
		select {
		case s.ch <- e:
		default:
			// Behind: cut off. Closing tells the reader to reconnect with
			// the last sequence number it saw.
			close(s.ch)
			delete(b.subs, s)
		}
	}
}

// Subscribe returns the kept entries after since, then a channel of new
// ones, closed if the subscriber falls behind or cancel is called. gap
// reports that entries after since are no longer kept. A since beyond the
// last entry (a client that watched a node before it restarted) counts as
// missing everything: it gets the whole history, and a gap. Starting from
// 0 is never a gap: a new watcher missed nothing it asked for.
func (b *Bus) Subscribe(since uint64, buffer int) (past []Entry, live <-chan Entry, gap bool, cancel func()) {
	if buffer <= 0 {
		buffer = 256
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if since > b.seq {
		since, gap = 0, true
	}
	for _, e := range b.history {
		if e.Seq > since {
			past = append(past, e)
		}
	}
	gap = gap || (since > 0 && len(b.history) > 0 && b.history[0].Seq > since+1)
	s := &subscriber{ch: make(chan Entry, buffer)}
	if b.closed {
		close(s.ch) // shutting down: history only
		return past, s.ch, gap, func() {}
	}
	b.subs[s] = struct{}{}
	return past, s.ch, gap, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if _, ok := b.subs[s]; ok {
			delete(b.subs, s)
			close(s.ch)
		}
	}
}

// Close ends every live subscription, so streams finish and the server
// serving them can shut down; later subscribers get the history only.
// Publishing still reaches the sinks (the log).
func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for s := range b.subs {
		close(s.ch)
		delete(b.subs, s)
	}
}

// Seq is the last sequence number published.
func (b *Bus) Seq() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.seq
}

// Summary is the shipper's notification about an alert: what it means for
// its own business. It may draw on private context, so it lives only in
// this node's activity (its log and control API), never in an envelope.
type Summary struct {
	Booking string `json:"booking"`
	AlertID string `json:"alertId"`
	Text    string `json:"text"`
	// Impact is "misses", "tight", "ok" or "unknown", against the
	// shipper's receiving slot or required date.
	Impact      string   `json:"impact"`
	GeneratedBy string   `json:"generatedBy"` // llm or template
	Cited       []string `json:"cited,omitempty"`
	// UnheldCauses are cause event IDs the alert cites that this shipper
	// holds no signed copy of.
	UnheldCauses []string `json:"unheldCauses,omitempty"`
}
