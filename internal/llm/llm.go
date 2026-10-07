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

// Package llm is the provider-neutral model interface (docs/DESIGN.md,
// A.10). Each Generate call is one turn; the caller owns any loop, and
// everything a model returns is untrusted input to validate.
//
// Implementations: fake (scripted, for tests and rehearsal) now; the
// Anthropic adapter and others plug in behind the same interface.
package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// Role is who said a message.
type Role string

// Roles.
const (
	User      Role = "user"
	Assistant Role = "assistant"
)

// Message is one turn of the conversation. An assistant message may carry
// tool calls; the user message after it carries their results.
type Message struct {
	Role        Role
	Text        string
	ToolCalls   []ToolCall   // assistant
	ToolResults []ToolResult // user
}

// GenerateRequest is one model turn's input.
type GenerateRequest struct {
	// System is the system prompt: instructions the model should follow.
	// Nothing untrusted belongs here.
	System   string
	Messages []Message
	// JSONSchema, if set, asks for a single JSON object matching it. It is
	// a request, not a guarantee: the caller still validates the output.
	JSONSchema json.RawMessage
	// MaxTokens caps the output (0: the provider's default).
	MaxTokens int
	// Tools the model may call. Calls come back in the response; the
	// caller validates them (ValidateCall) and decides whether to run them.
	Tools []Tool
}

// Usage is what a turn cost.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// GenerateResponse is one model turn's output.
type GenerateResponse struct {
	Text  string
	Usage Usage
	// Model is the model that answered, as the provider names it.
	Model string
	// ToolCalls are the tools the model asked to call, in order.
	ToolCalls []ToolCall
	// StopReason is why the turn ended: StopEnd, StopTool, StopMaxTokens,
	// or a provider's own reason.
	StopReason string
}

// Stop reasons.
const (
	StopEnd       = "end_turn"
	StopTool      = "tool_use"
	StopMaxTokens = "max_tokens"
)

// Model runs model turns.
type Model interface {
	// Name identifies the provider and model, for the node's log.
	Name() string
	// Generate runs one turn.
	Generate(ctx context.Context, req GenerateRequest) (GenerateResponse, error)
}

// PromptHash identifies a request's prompt in logs without logging it: the
// first 16 hex digits of the SHA-256 of the system prompt and messages.
func PromptHash(req GenerateRequest) string {
	h := sha256.New()
	h.Write([]byte(req.System))
	for _, m := range req.Messages {
		h.Write([]byte{0})
		h.Write([]byte(m.Role))
		h.Write([]byte{0})
		h.Write([]byte(m.Text))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// StripCodeFence removes one Markdown code fence wrapping s, which models
// often put around JSON even when asked not to. Anything else is returned
// trimmed and unchanged.
func StripCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") || !strings.HasSuffix(s, "```") || len(s) < 6 {
		return s
	}
	body := s[3 : len(s)-3]
	if i := strings.IndexByte(body, '\n'); i >= 0 && !strings.ContainsAny(body[:i], "{[\"") {
		body = body[i+1:] // the fence's info string, e.g. "json"
	}
	return strings.TrimSpace(body)
}
