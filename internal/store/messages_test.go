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
	"strings"
	"testing"

	"github.com/go-steer/supercargo/pkg/envelope"
)

func TestMessagesFilter(t *testing.T) {
	l := NewMessages(0, 0)
	l.Add(Message{Side: "operator", Direction: "in", Verified: true, Conversation: "c1", Wire: "a"})
	l.Add(Message{Side: "customer", Direction: "out", Verified: true, Conversation: "c2", Wire: "b"})
	l.Add(Message{Direction: "in", Outcome: "malformed", Wire: "junk"}) // the whole node's
	l.Add(Message{Side: "operator", Direction: "out", Verified: true, Conversation: "c1", Wire: "c"})
	wires := func(ms []Message) string {
		var w []string
		for _, m := range ms {
			w = append(w, m.Wire)
		}
		return strings.Join(w, ",")
	}
	for name, tc := range map[string]struct {
		f    MessageFilter
		want string
	}{
		"everything":        {MessageFilter{}, "a,b,junk,c"},
		"operator side":     {MessageFilter{Sides: []string{"operator"}}, "a,junk,c"},
		"customer side":     {MessageFilter{Sides: []string{"customer"}}, "b,junk"},
		"one conversation":  {MessageFilter{Conversation: "c1"}, "a,c"},
		"since":             {MessageFilter{Since: 2}, "junk,c"},
		"newest two":        {MessageFilter{Limit: 2}, "junk,c"},
		"side and since":    {MessageFilter{Sides: []string{"customer"}, Since: 2}, "junk"},
		"an unknown side":   {MessageFilter{Sides: []string{"nobody"}}, "junk"},
		"nothing after end": {MessageFilter{Since: 9}, ""},
	} {
		if got := wires(l.List(tc.f)); got != tc.want {
			t.Errorf("%s: %s, want %s", name, got, tc.want)
		}
	}
	if m, ok := l.Get(2, []string{"customer"}); !ok || m.Wire != "b" {
		t.Errorf("Get(2) as customer = %+v, %v", m, ok)
	}
	if _, ok := l.Get(2, []string{"operator"}); ok {
		t.Error("Get gave the operator side a customer-side message")
	}
	if _, ok := l.Get(3, []string{"operator"}); !ok {
		t.Error("Get kept the whole node's message from a side")
	}
	if _, ok := l.Get(99, nil); ok {
		t.Error("Get found a message never added")
	}
}

// TestMessagesBounded: the log keeps at most keep messages and budget
// bytes, dropping the oldest, and always the newest.
func TestMessagesBounded(t *testing.T) {
	l := NewMessages(3, 0)
	for range 5 {
		l.Add(Message{Wire: "x"})
	}
	if ms := l.List(MessageFilter{}); len(ms) != 3 || ms[0].Seq != 3 {
		t.Errorf("by count: %d kept, first %d", len(ms), ms[0].Seq)
	}
	l = NewMessages(100, 3100) // 1,512 bytes each with overhead: two fit
	for range 5 {
		l.Add(Message{Wire: strings.Repeat("w", 1000)})
	}
	if ms := l.List(MessageFilter{}); len(ms) != 2 || ms[1].Seq != 5 {
		t.Errorf("by bytes: %d kept", len(ms))
	}
	l.Add(Message{Wire: strings.Repeat("w", envelope.MaxSize+10)})
	if ms := l.List(MessageFilter{}); len(ms) != 1 || len(ms[0].Wire) != envelope.MaxSize {
		t.Errorf("an oversize message: %d kept, %d bytes", len(ms), len(ms[0].Wire))
	}
}

// TestMessagesSanitized: what a sender controls can't break a reader or
// pass as proven.
func TestMessagesSanitized(t *testing.T) {
	l := NewMessages(0, 0)
	m := l.Add(Message{Direction: "in", Verified: false, Body: json.RawMessage(`{"forged":true}`), Wire: "a\xffb"})
	if m.Body != nil || m.Wire != "a�b" {
		t.Errorf("unverified inbound: body %s, wire %q", m.Body, m.Wire)
	}
	if m := l.Add(Message{Direction: "out", Verified: true, Body: json.RawMessage(`{"cut`)}); m.Body != nil {
		t.Errorf("invalid JSON body kept: %s", m.Body)
	}
	// The log holds its own copies.
	ref := "OB-1"
	h := envelope.Header{Intent: "ulp.tnt.event", BookingRef: &ref, Dialect: &envelope.Dialect{Standard: "DCSA_TNT"}}
	body := json.RawMessage(`{"a":1}`)
	l.Add(Message{Direction: "in", Verified: true, Header: &h, Body: body})
	ref, body[1], h.Dialect.Standard = "XX", 'z', "OTHER"
	got := l.List(MessageFilter{Since: 2})[0]
	if *got.Header.BookingRef != "OB-1" || string(got.Body) != `{"a":1}` || got.Header.Dialect.Standard != "DCSA_TNT" {
		t.Errorf("the log shares the caller's memory: %s %s", *got.Header.BookingRef, got.Body)
	}
}
