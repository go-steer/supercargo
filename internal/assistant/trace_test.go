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
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/go-steer/supercargo/internal/llm"
	"github.com/go-steer/supercargo/internal/llm/fake"
	"github.com/go-steer/supercargo/internal/store"
)

// TestTurnTrace: a turn is a span, with each model call and each tool
// call under it; a refused tool call is an error.
func TestTurnTrace(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp)))
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })

	a, _, _ := newAssistant(FakeModel(func() time.Time { return thursday }), &node{})
	if _, err := a.Turn(context.Background(), &Conversation{}, "What can ocean-blue.example do?"); err != nil {
		t.Fatal(err)
	}
	m := fake.Turns(llm.GenerateResponse{ToolCalls: []llm.ToolCall{fake.Call("1", ToolGetProfile, `{"domain":"mallory.example"}`)}},
		llm.GenerateResponse{Text: "OK."})
	a, _, _ = newAssistant(m, &node{})
	if _, err := a.Turn(context.Background(), &Conversation{}, "Look at ocean-blue.example"); err != nil {
		t.Fatal(err)
	}
	// A refusal that names a private date: the span says "refused", not why.
	m = fake.Turns(llm.GenerateResponse{ToolCalls: []llm.ToolCall{
		fake.Call("1", ToolAsk, `{"bookingRef":"OB-BKG-1042","question":"We need it by 2026-10-15."}`),
		fake.Call("2", "send_booking", `{}`)}}, llm.GenerateResponse{Text: "OK."})
	a, _, _ = newAssistant(m, &node{shipments: []store.Summary{{Ref: "OB-BKG-1042", Operator: "ocean-blue.example"}}})
	a.Private = []string{"2026-10-15"}
	if _, err := a.Turn(context.Background(), &Conversation{}, "Ask them"); err != nil {
		t.Fatal(err)
	}
	a, _, _ = newAssistant(fake.Failing(fake.ErrUnavailable), &node{})
	if _, err := a.Turn(context.Background(), &Conversation{}, "Hi"); !errors.Is(err, fake.ErrUnavailable) {
		t.Fatal(err)
	}

	spans := exp.GetSpans()
	byID := map[string]tracetest.SpanStub{}
	for _, s := range spans {
		byID[s.SpanContext.SpanID().String()] = s
	}
	parent := func(s tracetest.SpanStub) string { return byID[s.Parent.SpanID().String()].Name }
	counts := map[string]int{}
	refused, failedTurn := false, false
	for _, s := range spans {
		counts[s.Name+" < "+parent(s)]++
		if s.Name == "execute_tool get_operator_profile" && s.Status.Code == codes.Error {
			refused = true
		}
		if s.Name == "ulp.assistant.turn" && s.Status.Code == codes.Error {
			failedTurn = true
		}
	}
	for _, s := range spans {
		dump := fmt.Sprintf("%v %v %v", s.Name, s.Attributes, s.Status)
		if strings.Contains(dump, "2026-10-15") || strings.Contains(dump, "send_booking") || strings.Contains(dump, "mallory") {
			t.Errorf("span %s carries %s", s.Name, dump)
		}
	}
	if s := findSpan(spans, "execute_tool ask_operator"); s.Status.Description != "refused" {
		t.Errorf("refused ask %+v", s.Status)
	}
	if s := findSpan(spans, "execute_tool unknown"); s.Status.Description != "unknown_tool" {
		t.Errorf("unknown tool %+v", s.Status)
	}
	if counts["ulp.assistant.turn < "] != 4 || counts["chat fake < ulp.assistant.turn"] != 7 || counts["execute_tool get_operator_profile < ulp.assistant.turn"] != 2 {
		t.Errorf("spans %v", counts)
	}
	if !refused || !failedTurn {
		t.Errorf("refused tool %v, failed turn %v", refused, failedTurn)
	}
}

func findSpan(spans tracetest.SpanStubs, name string) tracetest.SpanStub {
	for _, s := range spans {
		if s.Name == name {
			return s
		}
	}
	return tracetest.SpanStub{}
}
