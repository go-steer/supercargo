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

package telemetry

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/go-steer/supercargo/internal/config"
)

// collector accepts OTLP/HTTP trace exports and keeps their bodies.
type collector struct {
	mu     sync.Mutex
	bodies [][]byte
	paths  []string
}

func (c *collector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasSuffix(r.URL.Path, "/v1/traces") && !strings.HasSuffix(r.URL.Path, "/v1/metrics") {
		http.NotFound(w, r)
		return
	}
	b, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	c.bodies = append(c.bodies, b)
	c.paths = append(c.paths, r.URL.Path)
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/x-protobuf")
}

func (c *collector) got() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return bytes.Join(c.bodies, nil)
}

func clearEnv(t *testing.T) {
	for _, k := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_PROTOCOL", "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL",
		"OTEL_SDK_DISABLED", "OTEL_TRACES_EXPORTER", "OTEL_METRICS_EXPORTER"} {
		t.Setenv(k, "")
	}
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })
}

func TestOffByDefault(t *testing.T) {
	clearEnv(t)
	before := otel.GetTracerProvider()
	shutdown, err := Setup(context.Background(), nil, "cymbal-shoes.example")
	if err != nil || shutdown(context.Background()) != nil || otel.GetTracerProvider() != before {
		t.Errorf("Setup with nothing configured: %v", err)
	}
}

// TestExport: a span reaches the collector, named after the node.
func TestExport(t *testing.T) {
	clearEnv(t)
	col := &collector{}
	srv := httptest.NewServer(col)
	defer srv.Close()
	for name, c := range map[string]*config.Telemetry{
		"url":       {Protocol: "http", Endpoint: srv.URL + "/v1/traces"},
		"bare url":  {Protocol: "http", Endpoint: srv.URL, Insecure: true},
		"host:port": {Protocol: "http", Endpoint: strings.TrimPrefix(srv.URL, "http://"), Insecure: true, ServiceName: "cymbal-dev"},
	} {
		col.bodies = nil
		shutdown, err := Setup(context.Background(), c, "cymbal-shoes.example")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		_, span := otel.Tracer("test").Start(context.Background(), "ulp.ingress")
		span.End()
		if err := shutdown(context.Background()); err != nil {
			t.Fatalf("%s: shutdown: %v", name, err)
		}
		want := "cymbal-shoes.example"
		if c.ServiceName != "" {
			want = c.ServiceName
		}
		if got := col.got(); !bytes.Contains(got, []byte("ulp.ingress")) || !bytes.Contains(got, []byte(want)) {
			t.Errorf("%s: the collector got %q", name, got)
		}
	}
}

// TestEnvironment: the standard variables turn export on without a
// telemetry section; a sample ratio of 0 keeps nothing.
func TestEnvironment(t *testing.T) {
	clearEnv(t)
	col := &collector{}
	srv := httptest.NewServer(col)
	defer srv.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL)
	if !Enabled(nil, func(k string) string { return map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "x"}[k] }) {
		t.Error("the traces endpoint variable")
	}
	shutdown, err := Setup(context.Background(), nil, "ocean-blue.example")
	if err != nil {
		t.Fatal(err)
	}
	_, span := otel.Tracer("test").Start(context.Background(), "ulp.egress")
	span.End()
	_ = shutdown(context.Background())
	if !bytes.Contains(col.got(), []byte("ocean-blue.example")) {
		t.Errorf("the collector got %q", col.got())
	}

	zero := 0.0
	col.bodies = nil
	shutdown, err = Setup(context.Background(), &config.Telemetry{Protocol: "http", Endpoint: srv.URL, SampleRatio: &zero}, "x")
	if err != nil {
		t.Fatal(err)
	}
	_, span = otel.Tracer("test").Start(context.Background(), "dropped")
	span.End()
	_ = shutdown(context.Background())
	if bytes.Contains(col.got(), []byte("dropped")) {
		t.Error("a 0 ratio exported a span")
	}
}

func TestOffSwitches(t *testing.T) {
	for _, env := range []map[string]string{{"OTEL_SDK_DISABLED": "true"}, {"OTEL_TRACES_EXPORTER": "none", "OTEL_METRICS_EXPORTER": "NONE"}} {
		if Enabled(&config.Telemetry{}, func(k string) string { return env[k] }) {
			t.Errorf("%v didn't turn export off", env)
		}
	}
}

// TestOneSignalOff: switching one signal off keeps the other.
func TestOneSignalOff(t *testing.T) {
	clearEnv(t)
	col := &collector{}
	srv := httptest.NewServer(col)
	defer srv.Close()
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	if !Enabled(&config.Telemetry{}, os.Getenv) {
		t.Fatal("metrics alone should still be on")
	}
	shutdown, err := Setup(context.Background(), &config.Telemetry{Protocol: "http", Endpoint: srv.URL, Insecure: true}, "x")
	if err != nil {
		t.Fatal(err)
	}
	_, span := otel.Tracer("test").Start(context.Background(), "not-exported")
	span.End()
	_ = shutdown(context.Background())
	col.mu.Lock()
	defer col.mu.Unlock()
	for _, p := range col.paths {
		if strings.HasSuffix(p, "/v1/traces") {
			t.Error("traces exported with OTEL_TRACES_EXPORTER=none")
		}
	}
}

func TestGRPC(t *testing.T) {
	clearEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "grpc")
	if protocolFromEnv() != "grpc" {
		t.Error("protocol from the environment")
	}
	// The gRPC exporter connects lazily: Setup succeeds without a collector.
	shutdown, err := Setup(context.Background(), &config.Telemetry{Protocol: "grpc", Endpoint: "127.0.0.1:1", Insecure: true}, "x")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = shutdown(ctx)
}

// TestSamplerIgnoresRemoteFlags: a remote parent's flag never decides; a
// local parent's does.
func TestSamplerIgnoresRemoteFlags(t *testing.T) {
	sc := func(sampled, remote bool) trace.SpanContext {
		var f trace.TraceFlags
		if sampled {
			f = trace.FlagsSampled
		}
		return trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}, TraceFlags: f, Remote: remote})
	}
	decide := func(s sdktrace.Sampler, parent trace.SpanContext) sdktrace.SamplingDecision {
		return s.ShouldSample(sdktrace.SamplingParameters{ParentContext: trace.ContextWithSpanContext(context.Background(), parent),
			TraceID: trace.TraceID{1}, Name: "x"}).Decision
	}
	never, always := sampler(0), sampler(1)
	if decide(never, sc(true, true)) != sdktrace.Drop || decide(always, sc(false, true)) != sdktrace.RecordAndSample {
		t.Error("a remote flag decided")
	}
	if decide(never, sc(true, false)) != sdktrace.RecordAndSample || decide(always, sc(false, false)) != sdktrace.Drop {
		t.Error("a local parent's decision wasn't followed")
	}
}
