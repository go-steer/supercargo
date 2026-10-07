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

package llm

import (
	"context"
	"slices"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Model and tool spans (docs/DESIGN.md, A.16), after OpenTelemetry's
// GenAI semantic conventions: what was called, what it cost, and what
// the caller made of the reply. Never the prompt or the reply: they carry
// partner data.

const tracerName = "github.com/go-steer/supercargo/internal/llm"

// GenAI attribute keys (OpenTelemetry semantic conventions), and ours.
const (
	AttrOperation    = attribute.Key("gen_ai.operation.name")
	AttrProvider     = attribute.Key("gen_ai.provider.name")
	AttrRequestModel = attribute.Key("gen_ai.request.model")
	AttrModel        = attribute.Key("gen_ai.response.model")
	AttrInputTokens  = attribute.Key("gen_ai.usage.input_tokens")
	AttrOutputTokens = attribute.Key("gen_ai.usage.output_tokens")
	AttrFinish       = attribute.Key("gen_ai.response.finish_reasons")
	AttrToolName     = attribute.Key("gen_ai.tool.name")
	AttrToolCallID   = attribute.Key("gen_ai.tool.call.id")
	AttrAttempt      = attribute.Key("ulp.llm.attempt")
	AttrPromptHash   = attribute.Key("ulp.llm.prompt_hash")
	AttrSkills       = attribute.Key("ulp.llm.skills")
	AttrOutcome      = attribute.Key("ulp.llm.outcome")
)

// Call is the span of one model call, from StartCall to End.
type Call struct {
	span trace.Span
}

// StartCall starts the span for one model call: "chat <model>".
func StartCall(ctx context.Context, m Model, req GenerateRequest, attempt int, skills []string) (context.Context, *Call) {
	provider, model := splitName(m.Name())
	ctx, span := otel.Tracer(tracerName).Start(ctx, "chat "+model, trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(AttrOperation.String("chat"), AttrProvider.String(provider), AttrRequestModel.String(model),
			AttrAttempt.Int(attempt), AttrPromptHash.String(PromptHash(req))))
	if len(skills) > 0 {
		span.SetAttributes(AttrSkills.StringSlice(skills))
	}
	return ctx, &Call{span: span}
}

// End records the call's cost and what the caller made of the reply:
// outcome is "ok", "invalid" or "error". Only the outcome is recorded,
// never why: a validation message can quote the model's output, and a
// provider's error its request. The why stays in the node's own log.
func (c *Call) End(resp GenerateResponse, outcome string) {
	s := c.span
	s.SetAttributes(AttrInputTokens.Int(resp.Usage.InputTokens), AttrOutputTokens.Int(resp.Usage.OutputTokens),
		AttrOutcome.String(outcome))
	if resp.Model != "" {
		s.SetAttributes(AttrModel.String(resp.Model))
	}
	if resp.StopReason != "" {
		s.SetAttributes(AttrFinish.StringSlice([]string{resp.StopReason}))
	}
	if outcome != "ok" {
		s.SetStatus(codes.Error, outcome)
	}
	s.End()
}

// StartTool starts the span for one tool call: "execute_tool <name>",
// for a tool in tools. Any other name is the model's own text, so the
// span says "unknown" instead.
func StartTool(ctx context.Context, call ToolCall, tools []Tool) (context.Context, trace.Span) {
	name := "unknown"
	if slices.ContainsFunc(tools, func(t Tool) bool { return t.Name == call.Name }) {
		name = call.Name
	}
	return otel.Tracer(tracerName).Start(ctx, "execute_tool "+name, trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(AttrOperation.String("execute_tool"), AttrToolName.String(name), AttrToolCallID.String(bounded(call.ID))))
}

// splitName splits a model's Name ("anthropic:claude-…", "fake") into
// provider and model.
func splitName(name string) (string, string) {
	if p, m, ok := strings.Cut(name, ":"); ok {
		return p, m
	}
	return name, name
}

// bounded cuts s to 128 bytes on a rune boundary.
func bounded(s string) string {
	const limit = 128
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}
