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
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/go-steer/supercargo/internal/activity"
	"github.com/go-steer/supercargo/internal/config"
	"github.com/go-steer/supercargo/pkg/intents"
)

// collect returns the metrics m recorded, by name: each data point's
// attributes as "k=v,..." with its value.
func collect(t *testing.T, r *sdkmetric.ManualReader) map[string]map[string]float64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]float64{}
	key := func(s attribute.Set) string {
		var parts []string
		for _, kv := range s.ToSlice() {
			if kv.Value.String() != "" {
				parts = append(parts, string(kv.Key)+"="+kv.Value.String())
			}
		}
		return strings.Join(parts, ",")
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			pts := map[string]float64{}
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, p := range d.DataPoints {
					pts[key(p.Attributes)] = float64(p.Value)
				}
			case metricdata.Histogram[float64]:
				for _, p := range d.DataPoints {
					pts[key(p.Attributes)] = p.Sum
				}
			case metricdata.Histogram[int64]:
				for _, p := range d.DataPoints {
					pts[key(p.Attributes)] = float64(p.Sum)
				}
			case metricdata.Gauge[int64]:
				for _, p := range d.DataPoints {
					pts[key(p.Attributes)] = float64(p.Value)
				}
			}
			out[m.Name] = pts
		}
	}
	return out
}

func TestMetrics(t *testing.T) {
	r := sdkmetric.NewManualReader()
	meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(r)).Meter("test")
	m, err := NewMetrics(meter, []string{"cymbal-shoes.example"})
	if err != nil {
		t.Fatal(err)
	}
	pending, voyages := 3, 1
	reg, err := Gauges(meter, func() int { return pending }, func() int { return voyages })
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reg.Unregister() }()
	for _, e := range []activity.Entry{
		{Envelope: &activity.Envelope{Direction: "in", Intent: "ulp.tnt.inquiry", Sender: "cymbal-shoes.example", Via: "a2a", Verified: true, Outcome: "accepted"}},
		{Envelope: &activity.Envelope{Direction: "in", Intent: "ulp.tnt.inquiry", Sender: "mallory.example", Via: "a2a", Verified: true, Outcome: "rejected", Code: "forbidden", MessageID: "m-1"}},
		{Envelope: &activity.Envelope{Direction: "in", Intent: "ulp.made.up", Sender: "x.example", Outcome: "rejected", Code: "unsupported_intent"}},
		{Envelope: &activity.Envelope{Direction: "out", Intent: "ulp.tnt.event", Receiver: "cymbal-shoes.example", Via: "rest", Verified: true, Outcome: "sent"}},
		{Model: &activity.Model{Booking: "OB-BKG-1042", Model: "anthropic:claude-x", LatencyMS: 1500, InputTokens: 100, OutputTokens: 20, Outcome: "invalid"}},
		{Model: &activity.Model{Model: "fake", LatencyMS: 2, InputTokens: 5, OutputTokens: 1, Outcome: "ok"}},
		{Summary: &activity.Summary{Booking: "OB-BKG-1042", GeneratedBy: "llm", Impact: "misses"}},
		{Record: &activity.Record{Action: activity.ActionPublished, Booking: "OB-BKG-1042", Alert: &intents.TnTAlertBody{}}},
		{Record: &activity.Record{Action: activity.ActionRecorded, Booking: "OB-BKG-1042", Event: &intents.TnTEventBody{}}},
		{Push: &activity.Push{Booking: "OB-BKG-1042", Problem: "dropped"}},
		{Voyage: &activity.Voyage{Booking: "OB-BKG-1042", Started: true}},
	} {
		m.Observe(e)
	}
	m.Retry()
	got := collect(t, r)
	want := map[string]map[string]float64{
		"ulp.envelopes": {
			"ulp.direction=in,ulp.intent=ulp.tnt.inquiry,ulp.outcome=accepted,ulp.peer=cymbal-shoes.example,ulp.verified=true,ulp.via=a2a":     1,
			"ulp.code=forbidden,ulp.direction=in,ulp.intent=ulp.tnt.inquiry,ulp.outcome=rejected,ulp.peer=other,ulp.verified=true,ulp.via=a2a": 1,
			"ulp.code=unsupported_intent,ulp.direction=in,ulp.intent=other,ulp.outcome=rejected,ulp.peer=other,ulp.verified=false":             1,
			"ulp.direction=out,ulp.intent=ulp.tnt.event,ulp.outcome=sent,ulp.peer=cymbal-shoes.example,ulp.verified=true,ulp.via=rest":         1,
		},
		"ulp.llm.calls": {
			"gen_ai.provider.name=anthropic,gen_ai.request.model=claude-x,ulp.llm.outcome=invalid": 1,
			"gen_ai.provider.name=fake,gen_ai.request.model=fake,ulp.llm.outcome=ok":               1,
		},
		"gen_ai.client.operation.duration": {"gen_ai.provider.name=anthropic,gen_ai.request.model=claude-x": 1.5,
			"gen_ai.provider.name=fake,gen_ai.request.model=fake": 0.002},
		"gen_ai.client.token.usage": {
			"gen_ai.provider.name=anthropic,gen_ai.request.model=claude-x,gen_ai.token.type=input":  100,
			"gen_ai.provider.name=anthropic,gen_ai.request.model=claude-x,gen_ai.token.type=output": 20,
			"gen_ai.provider.name=fake,gen_ai.request.model=fake,gen_ai.token.type=input":           5,
			"gen_ai.provider.name=fake,gen_ai.request.model=fake,gen_ai.token.type=output":          1,
		},
		"ulp.alert.summaries":   {"ulp.generated_by=llm,ulp.impact=misses": 1},
		"ulp.records":           {"ulp.action=published,ulp.kind=alert": 1, "ulp.action=recorded,ulp.kind=event": 1},
		"ulp.publisher.drops":   {"ulp.problem=dropped": 1},
		"ulp.publisher.retries": {"": 1},
		"ulp.publisher.pending": {"": 3},
		"ulp.simulator.voyages": {"": 1},
	}
	for name, pts := range want {
		for k, v := range pts {
			if got[name][k] != v {
				t.Errorf("%s{%s} = %v, want %v (have %v)", name, k, got[name][k], v, got[name])
			}
		}
		if len(got[name]) != len(pts) {
			t.Errorf("%s has %d series, want %d: %v", name, len(got[name]), len(pts), got[name])
		}
	}
	// No booking reference or message ID is ever a label.
	for name, pts := range got {
		for k := range pts {
			if strings.Contains(k, "OB-BKG") || strings.Contains(k, "m-1") || strings.Contains(k, "mallory") {
				t.Errorf("%s has a high-cardinality label: %s", name, k)
			}
		}
	}
	if reg, err := Gauges(meter, nil, nil); err != nil || reg != nil {
		t.Error(err)
	}
}

// TestMetricsExport: metrics reach the collector, at the metrics endpoint
// when one is set.
func TestMetricsExport(t *testing.T) {
	clearEnv(t)
	col := &collector{}
	srv := httptest.NewServer(col)
	defer srv.Close()
	for name, c := range map[string]*config.Telemetry{
		"same collector":    {Protocol: "http", Endpoint: srv.URL, Insecure: true},
		"traces URL":        {Protocol: "http", Endpoint: srv.URL + "/otlp/v1/traces", Insecure: true},
		"metrics endpoint":  {Protocol: "http", Endpoint: "127.0.0.1:1", Insecure: true, MetricsEndpoint: srv.URL + "/v1/metrics"},
		"host:port metrics": {Protocol: "http", Endpoint: "127.0.0.1:1", Insecure: true, MetricsEndpoint: strings.TrimPrefix(srv.URL, "http://")},
	} {
		col.bodies, col.paths = nil, nil
		shutdown, err := Setup(context.Background(), c, "ocean-blue.example")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		m, err := NewMetrics(otel.Meter("test"), nil)
		if err != nil {
			t.Fatal(err)
		}
		m.Observe(activity.Entry{Envelope: &activity.Envelope{Direction: "in", Outcome: "malformed"}})
		_ = shutdown(context.Background())
		if !bytes.Contains(col.got(), []byte("ulp.envelopes")) {
			t.Errorf("%s: the collector got no metrics", name)
		}
		col.mu.Lock()
		if len(col.paths) == 0 || !strings.HasSuffix(col.paths[len(col.paths)-1], "/v1/metrics") {
			t.Errorf("%s: metrics went to %v", name, col.paths)
		}
		col.mu.Unlock()
	}
}
