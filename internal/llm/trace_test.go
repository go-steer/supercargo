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

package llm_test

import (
	"context"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/go-steer/supercargo/internal/llm"
)

type named string

func (n named) Name() string { return string(n) }
func (named) Generate(context.Context, llm.GenerateRequest) (llm.GenerateResponse, error) {
	return llm.GenerateResponse{}, nil
}

func recording(t *testing.T) *tracetest.InMemoryExporter {
	exp := tracetest.NewInMemoryExporter()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp)))
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })
	return exp
}

func attrs(s tracetest.SpanStub) map[string]string {
	out := map[string]string{}
	for _, a := range s.Attributes {
		out[string(a.Key)] = a.Value.String()
	}
	return out
}

func TestCallSpans(t *testing.T) {
	exp := recording(t)
	req := llm.GenerateRequest{System: "secret system prompt", Messages: []llm.Message{{Role: llm.User, Text: "partner question"}}}
	resp := llm.GenerateResponse{Text: "the answer", Model: "claude-x-2026", StopReason: llm.StopEnd, Usage: llm.Usage{InputTokens: 120, OutputTokens: 40}}

	_, c := llm.StartCall(context.Background(), named("anthropic:claude-x"), req, 2, []string{"shipment-inquiry@ab12"})
	c.End(resp, "ok")
	_, c = llm.StartCall(context.Background(), named("fake"), req, 1, nil)
	c.End(resp, "invalid")
	_, c = llm.StartCall(context.Background(), named("fake"), req, 1, nil)
	c.End(llm.GenerateResponse{}, "error")

	spans := exp.GetSpans()
	if len(spans) != 3 {
		t.Fatalf("%d spans", len(spans))
	}
	ok := attrs(spans[0])
	if spans[0].Name != "chat claude-x" || ok["gen_ai.provider.name"] != "anthropic" || ok["gen_ai.request.model"] != "claude-x" ||
		ok["gen_ai.response.model"] != "claude-x-2026" || ok["gen_ai.usage.input_tokens"] != "120" || ok["gen_ai.usage.output_tokens"] != "40" ||
		ok["ulp.llm.attempt"] != "2" || ok["ulp.llm.skills"] != `["shipment-inquiry@ab12"]` || ok["gen_ai.response.finish_reasons"] != `["end_turn"]` ||
		spans[0].Status.Code == codes.Error {
		t.Errorf("ok span %s %v", spans[0].Name, ok)
	}
	bad := attrs(spans[1])
	if spans[1].Name != "chat fake" || bad["ulp.llm.outcome"] != "invalid" || spans[1].Status.Description != "invalid" {
		t.Errorf("invalid span %v %+v", bad, spans[1].Status)
	}
	if spans[2].Status.Description != "error" {
		t.Errorf("error span %+v", spans[2].Status)
	}
	for _, s := range spans {
		for _, a := range s.Attributes {
			if v := a.Value.String(); strings.Contains(v, "secret") || strings.Contains(v, "partner question") || strings.Contains(v, "the answer") {
				t.Errorf("%s carries content: %s=%s", s.Name, a.Key, v)
			}
		}
	}
}

func TestToolSpan(t *testing.T) {
	exp := recording(t)
	tools := []llm.Tool{{Name: "get_events"}}
	_, span := llm.StartTool(context.Background(), llm.ToolCall{ID: "c1", Name: "get_events"}, tools)
	span.End()
	_, span = llm.StartTool(context.Background(), llm.ToolCall{ID: "c" + strings.Repeat("é", 100), Name: "send_booking now"}, tools)
	span.End()
	s := exp.GetSpans()
	if a := attrs(s[0]); s[0].Name != "execute_tool get_events" || a["gen_ai.operation.name"] != "execute_tool" || a["gen_ai.tool.call.id"] != "c1" {
		t.Errorf("tool span %s %v", s[0].Name, a)
	}
	if a := attrs(s[1]); s[1].Name != "execute_tool unknown" || a["gen_ai.tool.name"] != "unknown" || len(a["gen_ai.tool.call.id"]) > 135 {
		t.Errorf("an unknown tool's span %s %v", s[1].Name, a)
	}
}
