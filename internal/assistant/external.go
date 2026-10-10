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
// our bookings. The caller bounds the calls that reach other companies.

// External is an outside agent's session: its own conversation state (the
// partners it found). How much it may reach other companies is the
// caller's to bound, node-wide (internal/mcpserver).
type External struct {
	conv Conversation
}

// NewExternal starts an outside agent's session.
func NewExternal() *External {
	e := &External{}
	e.conv.external = true
	return e
}

// ToolOutcome is one tool call run for an outside agent: the result (or
// the refusal, IsError) as the model would see it, the draft it made, and
// a refusal's category (Status: bad_input, unknown_tool, refused or
// failed), which is all a log may record: the refusal's text can quote
// the call's arguments, or a private date it mentioned.
type ToolOutcome struct {
	Content string
	IsError bool
	Draft   *DraftResult
	Status  string
}

// ToolList is this assistant's tools, for an outside agent to call: the
// ones RunTool validates against, so a tool not listed never runs.
func (a *Assistant) ToolList() []llm.Tool { return a.tools() }

// RunTool runs one tool call for an outside agent's session e.
func (a *Assistant) RunTool(ctx context.Context, e *External, name string, input json.RawMessage) ToolOutcome {
	call := llm.ToolCall{ID: "mcp-1", Name: name, Input: input}
	t := &turnState{asked: map[string]bool{}, statused: map[string]bool{}}
	res := a.call(ctx, &e.conv, t, call, 0, false)
	return ToolOutcome{Content: res.Content, IsError: res.IsError, Draft: res.draft, Status: res.status}
}

// Outbound reports whether a tool reaches another company (a signed
// request, or a profile fetch): the ones an outside agent's calls must be
// bounded on.
func Outbound(name string) bool {
	switch name {
	case ToolAsk, ToolStatus, ToolGetProfile, ToolFind:
		return true
	}
	return false
}
