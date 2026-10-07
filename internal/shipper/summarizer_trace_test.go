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

package shipper

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"
)

// TestSummarizeTrace: the summary is a span with the cause check as an
// event and the model call under it.
func TestSummarizeTrace(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp)))
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })
	alert := delayAlert("2026-10-16T08:00:00Z", "EV-1", "EV-404")
	s, b := summarizerWith(t, FakeSummaryModel(), alert)
	s.Summarize(context.Background(), b, alert)

	spans := exp.GetSpans()
	var root, chat tracetest.SpanStub
	for _, sp := range spans {
		switch sp.Name {
		case "ulp.alert.summarize":
			root = sp
		case "chat fake":
			chat = sp
		}
	}
	got := map[string]string{}
	for _, a := range root.Attributes {
		got[string(a.Key)] = a.Value.String()
	}
	if got["ulp.impact"] != "misses" || got["ulp.generated_by"] != "llm" || got["ulp.unheld_causes"] != "1" || got["ulp.alert_id"] != "AL-7" {
		t.Errorf("root %v", got)
	}
	if len(root.Events) != 1 || root.Events[0].Name != "cause check" {
		t.Errorf("events %+v", root.Events)
	}
	if chat.Parent.SpanID() != root.SpanContext.SpanID() {
		t.Error("the model call isn't under the summary")
	}
}
