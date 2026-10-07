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

package node_test

import (
	"context"
	"slices"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/go-steer/supercargo/pkg/intents"
)

// TestScene5Trace: the inquiry's trace on the operator runs from ingress
// through authorization and the handler to the model call (its tokens and
// outcome) and the signed reply.
func TestScene5Trace(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp)))
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })

	d := newDemo(t)
	b := booked(t, d, nil)
	for _, body := range []intents.Body{event("EV-1", "GTIN"), alert("AL-1")} {
		if err := d.publisher.Publish(b, body); err != nil {
			t.Fatal(err)
		}
	}
	waitRecords(t, d.shipLog, operatorDomain, b.Ref, 2)
	exp.Reset()
	if _, err := d.ship.Ask(context.Background(), operatorDomain, b.Ref, "What caused the delay?"); err != nil {
		t.Fatal(err)
	}
	spans := exp.GetSpans()
	paths := tree(spans)
	for _, want := range []string{"ulp.egress", "ulp.egress>ulp.response",
		"ulp.ingress>ulp.authorize", "ulp.ingress>ulp.handle>chat fake", "ulp.ingress>ulp.reply.seal"} {
		if !slices.Contains(paths, want) {
			t.Errorf("no %s in\n%v", want, paths)
		}
	}
	chat := find(spans, "chat fake")
	if attr(chat, "gen_ai.operation.name") != "chat" || attr(chat, "gen_ai.provider.name") != "fake" || attr(chat, "ulp.llm.outcome") != "ok" ||
		attr(chat, "gen_ai.usage.input_tokens") == "0" || attr(chat, "ulp.llm.prompt_hash") == "" || attr(chat, "ulp.llm.attempt") != "1" {
		t.Errorf("chat span %+v", chat.Attributes)
	}
	if h := find(spans, "ulp.handle"); attr(h, "ulp.generated_by") != "llm" {
		t.Errorf("handle span %+v", h.Attributes)
	}
	noBodies(t, spans, "What caused the delay", "Halifax")
}
