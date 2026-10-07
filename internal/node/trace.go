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

package node

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	noopmetric "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"

	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/transport/rest"
)

// Tracing (docs/DESIGN.md, A.16): one trace per inbound message, from
// ingress to the reply, with a span per pipeline step; one per outbound
// exchange. Spans carry the envelope's facts (intent, parties, binding,
// message ID, kid, the decision), never a body, a question or an answer.

// tracerName is the instrumentation scope.
const tracerName = "github.com/go-steer/supercargo/internal/node"

// Attribute keys.
const (
	AttrDirection = attribute.Key("ulp.direction")
	AttrIntent    = attribute.Key("ulp.intent")
	AttrSender    = attribute.Key("ulp.sender")
	AttrReceiver  = attribute.Key("ulp.receiver")
	AttrVia       = attribute.Key("ulp.via")
	AttrMessageID = attribute.Key("ulp.message_id")
	AttrKID       = attribute.Key("ulp.kid")
	AttrVerified  = attribute.Key("ulp.verified")
	AttrOutcome   = attribute.Key("ulp.outcome")
	AttrCode      = attribute.Key("ulp.code")
	AttrReason    = attribute.Key("ulp.reason")
	AttrDecision  = attribute.Key("ulp.decision")
)

// remoteKey carries a partner's trace context, from the HTTP headers.
type remoteKey struct{}

// WithRemoteTrace returns ctx carrying sc, the trace context a request
// arrived with (W3C traceparent, extracted by the HTTP layer). Process
// links its trace to it, or, for a peer TrustTrace trusts, continues it.
func WithRemoteTrace(ctx context.Context, sc trace.SpanContext) context.Context {
	if !sc.IsValid() {
		return ctx
	}
	return context.WithValue(ctx, remoteKey{}, sc)
}

// RemoteTrace is the trace context WithRemoteTrace put in ctx, if any.
func RemoteTrace(ctx context.Context) trace.SpanContext {
	sc, _ := ctx.Value(remoteKey{}).(trace.SpanContext)
	return sc
}

// ingressTrace starts an inbound message's trace. A partner's trace
// context is untrusted input: by default the message starts a new trace
// that links to it, so a partner can't graft our spans into its trace. A
// sender TrustTrace trusts is continued instead. The trust is on the
// claimed sender (the trace starts before verification): at worst a
// stranger grafts our spans onto a trace of its choosing in our own
// backend. Sampling never follows a remote flag (internal/telemetry), and
// the partner's tracestate is dropped either way.
func (n *Node) ingressTrace(ctx context.Context, sender string, start time.Time) (context.Context, trace.Span) {
	opts := []trace.SpanStartOption{trace.WithSpanKind(trace.SpanKindServer), trace.WithTimestamp(start)}
	remote := RemoteTrace(ctx)
	if remote.IsValid() {
		remote = trace.NewSpanContext(trace.SpanContextConfig{TraceID: remote.TraceID(), SpanID: remote.SpanID(),
			TraceFlags: remote.TraceFlags(), Remote: true})
	}
	switch {
	case remote.IsValid() && sender != "" && n.cfg.TrustTrace != nil && n.cfg.TrustTrace(sender):
		ctx = trace.ContextWithRemoteSpanContext(ctx, remote)
	case remote.IsValid():
		opts = append(opts, trace.WithNewRoot(), trace.WithLinks(trace.Link{SpanContext: remote}))
	default:
		opts = append(opts, trace.WithNewRoot())
	}
	return n.start(ctx, "ulp.ingress", opts...)
}

func (n *Node) start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	return n.tracer.Start(ctx, name, opts...)
}

// observe reports ev to the Observer and records it on the span in ctx.
// A refusal or failure marks the span as an error, with the refusal code.
func (n *Node) observe(ctx context.Context, ev Event) {
	n.cfg.Observer(ev)
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}
	attrs := []attribute.KeyValue{AttrDirection.String(ev.Direction), AttrOutcome.String(ev.Outcome), AttrVerified.Bool(ev.Verified)}
	for k, v := range map[attribute.Key]string{AttrIntent: ev.Intent, AttrSender: ev.Sender, AttrReceiver: ev.Receiver,
		AttrVia: ev.Via, AttrMessageID: ev.MessageID, AttrKID: ev.KID, AttrCode: ev.Code, AttrReason: bounded(ev.Reason)} {
		if v != "" {
			attrs = append(attrs, k.String(v))
		}
	}
	span.SetAttributes(attrs...)
	switch ev.Outcome {
	case "rejected", "malformed", "failed":
		desc := ev.Code
		if desc == "" {
			desc = ev.Outcome
		}
		span.SetStatus(codes.Error, desc)
	}
}

// fail marks span as failed with err's text as its status, and ends it.
func fail(span trace.Span, err error) {
	if err != nil {
		span.SetStatus(codes.Error, bounded(err.Error()))
	}
	span.End()
}

// newDuration is the histogram of the whole pipeline's latency, per
// message, on meter (nil: the global provider's).
func newDuration(meter metric.Meter) metric.Float64Histogram {
	if meter == nil {
		meter = otel.Meter(tracerName)
	}
	h, err := meter.Float64Histogram("ulp.pipeline.duration", metric.WithUnit("s"),
		metric.WithDescription("Time from an inbound message's arrival to the node's answer, by intent and answer."))
	if err != nil {
		h, _ = noopmetric.Meter{}.Float64Histogram("ulp.pipeline.duration")
	}
	return h
}

// recordDuration records a message's pipeline latency. The intent label
// is the claimed one only if the node serves it ("other" otherwise), and
// the answer is its HTTP status class: no label a stranger can multiply.
func (n *Node) recordDuration(ctx context.Context, start time.Time, u *envelope.Unverified, res rest.Result) {
	intent := "other"
	if u != nil {
		if _, ok := n.cfg.Handlers[u.ClaimedHeader().Intent]; ok {
			intent = u.ClaimedHeader().Intent
		}
	}
	answer := "reply"
	switch {
	case res.Status >= 500:
		answer = "5xx"
	case res.Status >= 400:
		answer = "4xx"
	case res.Response == nil:
		answer = "no_reply"
	}
	n.duration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attribute.String("ulp.intent", intent),
		attribute.String("ulp.answer", answer)))
}

func defaultTracer(t trace.Tracer) trace.Tracer {
	if t != nil {
		return t
	}
	return otel.Tracer(tracerName)
}
