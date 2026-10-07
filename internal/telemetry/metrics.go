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
	"context"
	"slices"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/go-steer/supercargo/internal/activity"
	"github.com/go-steer/supercargo/pkg/intents"
)

// Metrics turns the node's activity stream into OpenTelemetry metrics
// (docs/DESIGN.md, A.16). Labels are low-cardinality by construction: an
// intent the node doesn't know is "other", a peer not among the node's
// partners is "other", and no booking reference or message ID is ever a
// label.
type Metrics struct {
	peers []string

	envelopes metric.Int64Counter
	duration  metric.Float64Histogram
	tokens    metric.Int64Histogram
	calls     metric.Int64Counter
	summaries metric.Int64Counter
	records   metric.Int64Counter
	drops     metric.Int64Counter
	retries   metric.Int64Counter
}

// NewMetrics creates the instruments on meter. peers are the domains that
// may appear as labels (the node's partners).
func NewMetrics(meter metric.Meter, peers []string) (*Metrics, error) {
	m := &Metrics{peers: peers}
	var err error
	set := func(e error) {
		if err == nil {
			err = e
		}
	}
	m.envelopes, err = meter.Int64Counter("ulp.envelopes", metric.WithUnit("{envelope}"),
		metric.WithDescription("Envelopes the node decided about, by direction, intent, binding, outcome and refusal code."))
	var e error
	m.duration, e = meter.Float64Histogram("gen_ai.client.operation.duration", metric.WithUnit("s"),
		metric.WithDescription("Model call latency."))
	set(e)
	m.tokens, e = meter.Int64Histogram("gen_ai.client.token.usage", metric.WithUnit("{token}"),
		metric.WithDescription("Tokens per model call, by type (input, output)."))
	set(e)
	m.calls, e = meter.Int64Counter("ulp.llm.calls", metric.WithUnit("{call}"),
		metric.WithDescription("Model calls, by the caller's verdict on the reply (ok, invalid, error)."))
	set(e)
	m.summaries, e = meter.Int64Counter("ulp.alert.summaries", metric.WithUnit("{summary}"),
		metric.WithDescription("Alert summaries, by who wrote them (llm, template) and the impact."))
	set(e)
	m.records, e = meter.Int64Counter("ulp.records", metric.WithUnit("{record}"),
		metric.WithDescription("Events and alerts recorded (received) or published."))
	set(e)
	m.drops, e = meter.Int64Counter("ulp.publisher.drops", metric.WithUnit("{push}"),
		metric.WithDescription("Pushes given up on, or not queued."))
	set(e)
	m.retries, e = meter.Int64Counter("ulp.publisher.retries", metric.WithUnit("{push}"),
		metric.WithDescription("Push attempts repeated after a retryable failure."))
	set(e)
	return m, err
}

// peer is a domain as a label: itself if it's a partner, else "other".
func (m *Metrics) peer(d string) string {
	if d == "" {
		return ""
	}
	if slices.Contains(m.peers, d) {
		return d
	}
	return "other"
}

// intent is an intent as a label: itself if the node knows it.
func intent(i string) string {
	if i == "" {
		return ""
	}
	if intents.Supported(i) {
		return i
	}
	return "other"
}

// Observe records one activity entry: the activity bus's sink.
func (m *Metrics) Observe(e activity.Entry) {
	ctx := context.Background()
	switch {
	case e.Envelope != nil:
		v := e.Envelope
		peer := v.Sender
		if v.Direction == "out" {
			peer = v.Receiver
		}
		m.envelopes.Add(ctx, 1, metric.WithAttributes(attribute.String("ulp.direction", v.Direction),
			attribute.String("ulp.intent", intent(v.Intent)), attribute.String("ulp.via", v.Via),
			attribute.String("ulp.outcome", v.Outcome), attribute.String("ulp.code", v.Code),
			attribute.String("ulp.peer", m.peer(peer)), attribute.Bool("ulp.verified", v.Verified)))
	case e.Model != nil:
		v := e.Model
		provider, model, _ := strings.Cut(v.Model, ":")
		if model == "" {
			model = provider
		}
		attrs := []attribute.KeyValue{attribute.String("gen_ai.provider.name", provider), attribute.String("gen_ai.request.model", model)}
		m.duration.Record(ctx, float64(v.LatencyMS)/1000, metric.WithAttributes(attrs...))
		m.tokens.Record(ctx, int64(v.InputTokens), metric.WithAttributes(append(attrs, attribute.String("gen_ai.token.type", "input"))...))
		m.tokens.Record(ctx, int64(v.OutputTokens), metric.WithAttributes(append(attrs, attribute.String("gen_ai.token.type", "output"))...))
		m.calls.Add(ctx, 1, metric.WithAttributes(append(attrs, attribute.String("ulp.llm.outcome", v.Outcome))...))
	case e.Summary != nil:
		m.summaries.Add(ctx, 1, metric.WithAttributes(attribute.String("ulp.generated_by", e.Summary.GeneratedBy),
			attribute.String("ulp.impact", e.Summary.Impact)))
	case e.Record != nil:
		kind := "event"
		if e.Record.Alert != nil {
			kind = "alert"
		}
		m.records.Add(ctx, 1, metric.WithAttributes(attribute.String("ulp.action", e.Record.Action), attribute.String("ulp.kind", kind)))
	case e.Push != nil:
		m.drops.Add(ctx, 1, metric.WithAttributes(attribute.String("ulp.problem", e.Push.Problem)))
	}
}

// Retry counts a push about to be tried again (Publisher.OnRetry).
func (m *Metrics) Retry() {
	m.retries.Add(context.Background(), 1)
}

// Gauges registers observable gauges on meter: pushes waiting, and
// voyages in progress (nil skips one). Unregister the result at shutdown.
func Gauges(meter metric.Meter, pending, voyages func() int) (metric.Registration, error) {
	var insts []metric.Observable
	var pg, vg metric.Int64ObservableGauge
	var err error
	if pending != nil {
		if pg, err = meter.Int64ObservableGauge("ulp.publisher.pending", metric.WithUnit("{push}"),
			metric.WithDescription("Pushes queued for delivery.")); err != nil {
			return nil, err
		}
		insts = append(insts, pg)
	}
	if voyages != nil {
		if vg, err = meter.Int64ObservableGauge("ulp.simulator.voyages", metric.WithUnit("{voyage}"),
			metric.WithDescription("Voyages in progress.")); err != nil {
			return nil, err
		}
		insts = append(insts, vg)
	}
	if len(insts) == 0 {
		return nil, nil
	}
	return meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		if pending != nil {
			o.ObserveInt64(pg, int64(pending()))
		}
		if voyages != nil {
			o.ObserveInt64(vg, int64(voyages()))
		}
		return nil
	}, insts...)
}
