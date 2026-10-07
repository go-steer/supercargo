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
	"strings"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/go-steer/supercargo/internal/node"
)

// TestPipelineDuration: each inbound message's latency is recorded by
// intent (only one the node serves) and by answer class.
func TestPipelineDuration(t *testing.T) {
	r := sdkmetric.NewManualReader()
	op, cy := newParty(t, "ob-2026-1"), newParty(t, "cy-2026-1")
	cfg := baseConfig(t, op, staticKeys{shipperDomain + "/cy-2026-1": cy.v},
		handlerFunc{handle: func(context.Context, *node.Request) (*node.Reply, error) { return nil, nil }})
	cfg.Meter = sdkmetric.NewMeterProvider(sdkmetric.WithReader(r)).Meter("test")
	n, err := node.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	process(t, n, cy, op)                                // accepted, no reply
	n.Process(context.Background(), []byte("not a JWS")) // 400
	process(t, n, newParty(t, "cy-2026-1"), op)          // bad signature: 403

	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]uint64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "ulp.pipeline.duration" {
				continue
			}
			for _, p := range m.Data.(metricdata.Histogram[float64]).DataPoints {
				var parts []string
				for _, kv := range p.Attributes.ToSlice() {
					parts = append(parts, string(kv.Key)+"="+kv.Value.String())
				}
				got[strings.Join(parts, ",")] = p.Count
			}
		}
	}
	want := map[string]uint64{
		"ulp.answer=no_reply,ulp.intent=ulp.booking.request": 1,
		"ulp.answer=4xx,ulp.intent=other":                    1,
		"ulp.answer=4xx,ulp.intent=ulp.booking.request":      1,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %d, want %d (have %v)", k, got[k], v, got)
		}
	}
}
