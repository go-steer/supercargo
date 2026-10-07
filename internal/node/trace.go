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

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
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

func defaultTracer(t trace.Tracer) trace.Tracer {
	if t != nil {
		return t
	}
	return otel.Tracer(tracerName)
}
