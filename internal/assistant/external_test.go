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
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/llm/fake"
	"github.com/go-steer/supercargo/internal/store"
)

// TestExternal: an outside agent's session runs the same tools through
// the same checks, without the counterparty rule (it can't see the
// user's words) but with drafts only and a question rate limit; the rule
// still holds for the node's own conversations.
func TestExternal(t *testing.T) {
	n := &node{shipments: []store.Summary{{Ref: "OB-BKG-1042", Operator: "ocean-blue.example"}}}
	now := thursday
	a, tools, _ := newAssistant(nil, n)
	a.Now = func() time.Time { return now }
	e := NewExternal()
	ctx := context.Background()
	// A carrier nobody named: fine for an outside agent's draft (a person
	// confirms it), refused in our own chat.
	out := a.RunTool(ctx, e, ToolDraftBooking, json.RawMessage(draftJSON("")))
	if out.IsError || out.Draft == nil || n.called("book") {
		t.Fatalf("an outside draft: %+v", out)
	}
	if ev := oneCall(t, a, tools, &Conversation{}, "book it", fake.Call("1", ToolDraftBooking, draftJSON(""))); !strings.Contains(ev.Error, "hasn't named") {
		t.Errorf("the counterparty rule in our own chat: %q", ev.Error)
	}
	// The same checks on the bytes: refused.
	for _, bad := range []string{`{"bookingRef":"OB-BKG-1042","BookingRef":"x","question":"Why?"}`, `[1]`, `{"domain":"not a domain"}`} {
		name := ToolAsk
		if strings.Contains(bad, "domain") {
			name = ToolGetProfile
		}
		if out := a.RunTool(ctx, e, name, json.RawMessage(bad)); !out.IsError {
			t.Errorf("%s: accepted", bad)
		}
	}
	if out := a.RunTool(ctx, e, "send_booking", json.RawMessage(`{}`)); !out.IsError {
		t.Error("an unknown tool ran")
	}
	// Questions: ten in the window, then refused, then room again.
	ask := json.RawMessage(`{"bookingRef":"OB-BKG-1042","question":"Why is it late?"}`)
	for i := range MaxExternalAsks {
		if out := a.RunTool(ctx, e, ToolAsk, ask); out.IsError {
			t.Fatalf("ask %d: %s", i, out.Content)
		}
	}
	if out := a.RunTool(ctx, e, ToolAsk, ask); !out.IsError || !strings.Contains(out.Content, "10 questions") {
		t.Errorf("the eleventh: %+v", out)
	}
	// Another session has its own window.
	if out := a.RunTool(ctx, NewExternal(), ToolAsk, ask); out.IsError {
		t.Errorf("another session: %s", out.Content)
	}
	now = now.Add(ExternalAskWindow + time.Second)
	if out := a.RunTool(ctx, e, ToolAsk, ask); out.IsError {
		t.Errorf("after the window: %s", out.Content)
	}
	if got := len(a.ToolList()); got != len(Tools) {
		t.Errorf("ToolList: %d", got)
	}
}
