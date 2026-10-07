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

package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/go-steer/supercargo/internal/config"
	"github.com/go-steer/supercargo/internal/node"
)

// TestTracePropagation: an exchange's trace context goes out as W3C
// traceparent (no baggage), and comes in as a remote context for the
// pipeline to link to.
func TestTracePropagation(t *testing.T) {
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{9}, SpanID: trace.SpanID{8}, TraceFlags: trace.FlagsSampled})
	h := http.Header{}
	injectTrace(trace.ContextWithSpanContext(context.Background(), sc), h)
	if h.Get("traceparent") == "" || h.Get("baggage") != "" {
		t.Fatalf("headers %v", h)
	}

	var got trace.SpanContext
	handler := extractTrace(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = node.RemoteTrace(r.Context()) }))
	req := httptest.NewRequest(http.MethodPost, "/ulp", nil)
	req.Header = h
	req.Header.Set("baggage", "user=mallory")
	handler.ServeHTTP(httptest.NewRecorder(), req)
	if got.TraceID() != sc.TraceID() || got.SpanID() != sc.SpanID() || !got.IsRemote() {
		t.Errorf("extracted %+v", got)
	}
	// No header, or a malformed one: nothing.
	for _, tp := range []string{"", "garbage"} {
		got = trace.SpanContext{}
		req := httptest.NewRequest(http.MethodPost, "/ulp", nil)
		if tp != "" {
			req.Header.Set("traceparent", tp)
		}
		handler.ServeHTTP(httptest.NewRecorder(), req)
		if got.IsValid() {
			t.Errorf("%q gave %+v", tp, got)
		}
	}

	if trustTrace(nil) != nil || trustTrace(&config.Telemetry{}) != nil {
		t.Error("trust with no peers")
	}
	trust := trustTrace(&config.Telemetry{TrustPeers: []string{"ocean-blue.example"}})
	if !trust("ocean-blue.example") || trust("mallory.example") {
		t.Error("trustTrace")
	}
	t.Setenv("OTEL_SDK_DISABLED", "true")
	if trustTrace(&config.Telemetry{TrustPeers: []string{"ocean-blue.example"}}) != nil {
		t.Error("trust with telemetry off")
	}
}
