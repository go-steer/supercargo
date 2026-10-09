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
	"sync"
	"time"

	"github.com/go-steer/supercargo/internal/llm"
)

// Tools for an outside agent (an MCP client, docs/agent-runtime.md): the
// same tools, through the same checks (llm.ValidateCall on the exact
// argument bytes, then each tool's own rules), one call at a time, with
// no model of ours in the loop.
//
// One rule changes. The counterparty rule checks the user's own words,
// which the node never sees when the agent is elsewhere; for an outside
// caller it gives way to what already guards every effect: tools only
// draft, a person confirms each draft in the node's own UI (which marks a
// partner new to us), and a question goes only to the carrier of one of
// our bookings. Questions to carriers are also rate-limited per session.

// MaxExternalAsks bounds the questions to carriers one outside session
// sends in ExternalAskWindow.
const (
	MaxExternalAsks   = 10
	ExternalAskWindow = 10 * time.Minute
)

// External is an outside agent's session: its own conversation state (the
// partners it found) and its own rate limit.
type External struct {
	conv  Conversation
	askMu sync.Mutex // one question at a time, so the count holds
	mu    sync.Mutex
	asks  []time.Time
}

// NewExternal starts an outside agent's session.
func NewExternal() *External {
	e := &External{}
	e.conv.external = true
	return e
}

// ToolOutcome is one tool call run for an outside agent: the result (or
// the refusal, IsError) as the model would see it, and the draft it made.
type ToolOutcome struct {
	Content string
	IsError bool
	Draft   *DraftResult
}

// ToolList is this assistant's tools, for an outside agent to call.
func (a *Assistant) ToolList() []llm.Tool { return Tools }

// RunTool runs one tool call for an outside agent's session e.
func (a *Assistant) RunTool(ctx context.Context, e *External, name string, input json.RawMessage) ToolOutcome {
	if name == ToolAsk {
		// One question at a time per session, counted once it went out.
		e.askMu.Lock()
		defer e.askMu.Unlock()
		if !e.roomToAsk(a.now()) {
			return ToolOutcome{Content: "refused: at most 10 questions to carriers in 10 minutes from one session; try again later", IsError: true}
		}
	}
	call := llm.ToolCall{ID: "mcp-1", Name: name, Input: input}
	t := &turnState{asked: map[string]bool{}, statused: map[string]bool{}}
	res := a.call(ctx, &e.conv, t, call, 0, false)
	if name == ToolAsk && !res.IsError {
		e.mu.Lock()
		e.asks = append(e.asks, a.now())
		e.mu.Unlock()
	}
	return ToolOutcome{Content: res.Content, IsError: res.IsError, Draft: res.draft}
}

// roomToAsk: fewer than MaxExternalAsks questions went out in the window.
func (e *External) roomToAsk(now time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	kept := e.asks[:0]
	for _, t := range e.asks {
		if now.Sub(t) < ExternalAskWindow {
			kept = append(kept, t)
		}
	}
	e.asks = kept
	return len(e.asks) < MaxExternalAsks
}
