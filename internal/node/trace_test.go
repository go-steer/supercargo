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
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
)

// recorder returns a tracer that keeps every finished span.
func recorder() (trace.Tracer, *tracetest.InMemoryExporter) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	return tp.Tracer("test"), exp
}

// tree renders spans as "parent>child" paths, sorted, so a test can
// assert the shape.
func tree(spans tracetest.SpanStubs) []string {
	names := map[trace.SpanID]string{}
	parent := map[trace.SpanID]trace.SpanID{}
	for _, s := range spans {
		names[s.SpanContext.SpanID()] = s.Name
		parent[s.SpanContext.SpanID()] = s.Parent.SpanID()
	}
	var out []string
	for _, s := range spans {
		path := s.Name
		for p := parent[s.SpanContext.SpanID()]; p.IsValid(); p = parent[p] {
			path = names[p] + ">" + path
		}
		out = append(out, path)
	}
	slices.Sort(out)
	return out
}

func find(spans tracetest.SpanStubs, name string) tracetest.SpanStub {
	for _, s := range spans {
		if s.Name == name {
			return s
		}
	}
	return tracetest.SpanStub{}
}

func attr(s tracetest.SpanStub, key string) string {
	for _, a := range s.Attributes {
		if string(a.Key) == key {
			return a.Value.String()
		}
	}
	return ""
}

// noBodies: no span carries any part of a body.
func noBodies(t *testing.T, spans tracetest.SpanStubs, secrets ...string) {
	t.Helper()
	for _, s := range spans {
		dump := fmt.Sprintf("%v %v %v", s.Attributes, s.Events, s.Status)
		for _, secret := range secrets {
			if strings.Contains(dump, secret) {
				t.Errorf("span %s carries %q", s.Name, secret)
			}
		}
	}
}

// TestTraceIngress: one trace per inbound message, a span per pipeline
// step, the decision on the root; the reply's seal under it.
func TestTraceIngress(t *testing.T) {
	op, cy := newParty(t, "ob-2026-1"), newParty(t, "cy-2026-1")
	keys := staticKeys{shipperDomain + "/cy-2026-1": cy.v}
	tracer, exp := recorder()
	cfg := baseConfig(t, op, keys, handlerFunc{handle: func(context.Context, *node.Request) (*node.Reply, error) {
		return &node.Reply{Body: confirmationFor("OB-BKG-1"), BookingRef: "OB-BKG-1"}, nil
	}})
	cfg.Tracer = tracer
	n, err := node.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if e, raw := process(t, n, cy, op); e != nil || raw == nil {
		t.Fatalf("process: %+v", e)
	}
	spans := exp.GetSpans()
	want := []string{"ulp.ingress", "ulp.ingress>ulp.authorize", "ulp.ingress>ulp.decode", "ulp.ingress>ulp.handle", "ulp.ingress>ulp.parse",
		"ulp.ingress>ulp.replay", "ulp.ingress>ulp.reply.seal", "ulp.ingress>ulp.verify", "ulp.ingress>ulp.verify>ulp.key.resolve"}
	if got := tree(spans); !slices.Equal(got, want) {
		t.Errorf("span tree:\n%v\nwant\n%v", got, want)
	}
	root := find(spans, "ulp.ingress")
	if root.SpanKind != trace.SpanKindServer || attr(root, "ulp.outcome") != "accepted" || attr(root, "ulp.verified") != "true" ||
		attr(root, "ulp.intent") != intents.BookingRequest || attr(root, "ulp.sender") != shipperDomain || attr(root, "ulp.kid") != "cy-2026-1" ||
		attr(root, "ulp.message_id") == "" || root.Status.Code == codes.Error {
		t.Errorf("root span %+v", root)
	}
	if a := find(spans, "ulp.authorize"); attr(a, "ulp.decision") != "allowed" {
		t.Errorf("authorize %+v", a.Attributes)
	}
	if s := find(spans, "ulp.reply.seal"); attr(s, "ulp.direction") != "out" || attr(s, "ulp.outcome") != "sent" {
		t.Errorf("seal %+v", s.Attributes)
	}
	noBodies(t, spans, "Footwear", "6404", "OCBU")
}

func TestTraceRefusals(t *testing.T) {
	op, cy := newParty(t, "ob-2026-1"), newParty(t, "cy-2026-1")
	mallory := newParty(t, "cy-2026-1")
	keys := staticKeys{shipperDomain + "/cy-2026-1": cy.v}
	for name, c := range map[string]struct {
		signer     party
		authorize  func(context.Context, *node.Request) error
		raw        []byte
		failing    string // the span marked as failed
		code       string
		verified   string
		noDecision bool
	}{
		"denied": {signer: cy, authorize: func(context.Context, *node.Request) error {
			return node.Reject(intents.CodeForbidden, "No.", "not a partner")
		}, failing: "ulp.authorize", code: "forbidden", verified: "true"},
		"bad signature": {signer: mallory, failing: "ulp.verify", code: "bad_signature", verified: "false", noDecision: true},
		"malformed":     {raw: []byte("not a JWS"), failing: "ulp.parse", verified: "false", noDecision: true},
	} {
		tracer, exp := recorder()
		cfg := baseConfig(t, op, keys, handlerFunc{authorize: c.authorize, handle: func(context.Context, *node.Request) (*node.Reply, error) {
			return nil, nil
		}})
		cfg.Tracer = tracer
		n, err := node.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if c.raw != nil {
			n.Process(context.Background(), c.raw)
		} else {
			process(t, n, c.signer, op)
		}
		spans := exp.GetSpans()
		root := find(spans, "ulp.ingress")
		if root.Status.Code != codes.Error || attr(root, "ulp.verified") != c.verified || attr(root, "ulp.code") != c.code {
			t.Errorf("%s: root %+v %+v", name, root.Status, root.Attributes)
		}
		if f := find(spans, c.failing); f.Status.Code != codes.Error {
			t.Errorf("%s: %s not marked failed: %+v", name, c.failing, f.Status)
		}
		if c.noDecision && find(spans, "ulp.authorize").Name != "" {
			t.Errorf("%s: authorized before verifying", name)
		}
		if a := find(spans, "ulp.authorize"); !c.noDecision && (attr(a, "ulp.decision") != "denied" || attr(a, "ulp.code") != c.code) {
			t.Errorf("%s: authorize %+v", name, a.Attributes)
		}
	}
}

// TestTraceStopsWhereRefused: a replayed message stops at ulp.replay (no
// decode, no handler); an unknown key at ulp.key.resolve; a failing or
// panicking handler's span is ended as a failure.
func TestTraceStopsWhereRefused(t *testing.T) {
	op, cy := newParty(t, "ob-2026-1"), newParty(t, "cy-2026-1")
	keys := staticKeys{shipperDomain + "/cy-2026-1": cy.v}
	tracer, exp := recorder()
	cfg := baseConfig(t, op, keys, handlerFunc{handle: func(context.Context, *node.Request) (*node.Reply, error) { return nil, nil }})
	cfg.Tracer = tracer
	n, err := node.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := envelope.NewHeader(intents.BookingRequest, shipperDomain, operatorDomain, time.Now())
	b, _ := json.Marshal(bookingRequest())
	msg, _ := envelope.Seal(&envelope.Envelope{Header: h, Body: b}, "cy-2026-1", cy.signer)
	n.Process(context.Background(), msg)
	exp.Reset()
	n.Process(context.Background(), msg) // again: a replay
	spans := exp.GetSpans()
	if find(spans, "ulp.replay").Status.Code != codes.Error || find(spans, "ulp.decode").Name != "" || find(spans, "ulp.handle").Name != "" ||
		attr(find(spans, "ulp.ingress"), "ulp.code") != "replay" {
		t.Errorf("replay: %v", tree(spans))
	}

	exp.Reset()
	cfg.Keys = staticKeys{}
	cfg.Replay = store.NewReplay(store.ReplayOptions{})
	unknown, _ := node.New(cfg)
	unknown.Process(context.Background(), msg)
	spans = exp.GetSpans()
	if find(spans, "ulp.key.resolve").Status.Code != codes.Error || attr(find(spans, "ulp.ingress"), "ulp.code") != "unknown_key" {
		t.Errorf("unknown key: %v", tree(spans))
	}

	for name, h := range map[string]handlerFunc{
		"handler error":   {handle: func(context.Context, *node.Request) (*node.Reply, error) { return nil, fmt.Errorf("disk full") }},
		"handler panic":   {handle: func(context.Context, *node.Request) (*node.Reply, error) { panic("boom") }},
		"authorize panic": {authorize: func(context.Context, *node.Request) error { panic("boom") }, handle: func(context.Context, *node.Request) (*node.Reply, error) { return nil, nil }},
	} {
		exp.Reset()
		c := baseConfig(t, op, keys, h)
		c.Tracer = tracer
		hn, _ := node.New(c)
		process(t, hn, cy, op)
		spans := exp.GetSpans()
		step := "ulp.handle"
		if name == "authorize panic" {
			step = "ulp.authorize"
		}
		if find(spans, step).Status.Code != codes.Error || attr(find(spans, "ulp.ingress"), "ulp.code") != "internal" {
			t.Errorf("%s: %v %+v", name, tree(spans), find(spans, step).Status)
		}
	}
}

// TestTraceEgress: an exchange is a client span, with the response's
// checks under it.
func TestTraceEgress(t *testing.T) {
	op, cy := newParty(t, "ob-2026-1"), newParty(t, "cy-2026-1")
	opKeys := staticKeys{shipperDomain + "/cy-2026-1": cy.v}
	opNode, err := node.New(baseConfig(t, op, opKeys, handlerFunc{handle: func(context.Context, *node.Request) (*node.Reply, error) {
		return &node.Reply{Body: confirmationFor("OB-BKG-1"), BookingRef: "OB-BKG-1"}, nil
	}}))
	if err != nil {
		t.Fatal(err)
	}
	tracer, exp := recorder()
	var postedUnder trace.SpanContext
	shipper, err := node.New(node.Config{
		Domain: shipperDomain, Name: "Cymbal Shoes", Roles: []string{"shipper"}, KID: "cy-2026-1", Signer: cy.signer, PublicJWK: cy.jwk,
		Keys: staticKeys{operatorDomain + "/ob-2026-1": op.v}, Replay: store.NewReplay(store.ReplayOptions{}), Tracer: tracer,
		Poster: posterFunc(func(ctx context.Context, _ string, msg []byte) ([]byte, error) {
			postedUnder = trace.SpanContextFromContext(ctx)
			return opNode.Process(ctx, msg).Response, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	h := envelope.NewHeader(intents.BookingRequest, shipperDomain, operatorDomain, time.Now())
	if _, err := shipper.Exchange(context.Background(), h, bookingRequest(), intents.BookingConfirmation); err != nil {
		t.Fatal(err)
	}
	spans := exp.GetSpans()
	if got := tree(spans); !slices.Equal(got, []string{"ulp.egress", "ulp.egress>ulp.response"}) {
		t.Errorf("span tree %v", got)
	}
	eg := find(spans, "ulp.egress")
	if postedUnder.SpanID() != eg.SpanContext.SpanID() {
		t.Error("the transport didn't get the egress span's context")
	}
	if eg.SpanKind != trace.SpanKindClient || attr(eg, "ulp.outcome") != "sent" || attr(eg, "ulp.via") != "rest" || attr(eg, "ulp.receiver") != operatorDomain {
		t.Errorf("egress %+v", eg.Attributes)
	}
	if r := find(spans, "ulp.response"); attr(r, "ulp.outcome") != "response" || attr(r, "ulp.verified") != "true" {
		t.Errorf("response %+v", r.Attributes)
	}
	noBodies(t, spans, "Footwear", "6404")

	// A failed send marks the egress span.
	exp.Reset()
	failing, _ := node.New(node.Config{
		Domain: shipperDomain, Name: "Cymbal Shoes", Roles: []string{"shipper"}, KID: "cy-2026-1", Signer: cy.signer, PublicJWK: cy.jwk,
		Keys: staticKeys{}, Replay: store.NewReplay(store.ReplayOptions{}), Tracer: tracer,
		Poster: posterFunc(func(context.Context, string, []byte) ([]byte, error) { return nil, fmt.Errorf("connection refused") }),
	})
	if _, err := failing.Exchange(context.Background(), envelope.NewHeader(intents.BookingRequest, shipperDomain, operatorDomain, time.Now()),
		bookingRequest(), intents.BookingConfirmation); err == nil {
		t.Fatal("no error")
	}
	if eg := find(exp.GetSpans(), "ulp.egress"); eg.Status.Code != codes.Error || attr(eg, "ulp.outcome") != "failed" {
		t.Errorf("failed egress %+v %+v", eg.Status, eg.Attributes)
	}

	// A partner's refusal is recorded by its code, not its free text.
	exp.Reset()
	refusing, _ := node.New(baseConfig(t, op, opKeys, handlerFunc{authorize: func(context.Context, *node.Request) error {
		return node.Reject(intents.CodeForbidden, "Mallory says hello.", "no")
	}, handle: func(context.Context, *node.Request) (*node.Reply, error) { return nil, nil }}))
	asker, _ := node.New(node.Config{
		Domain: shipperDomain, Name: "Cymbal Shoes", Roles: []string{"shipper"}, KID: "cy-2026-1", Signer: cy.signer, PublicJWK: cy.jwk,
		Keys: staticKeys{operatorDomain + "/ob-2026-1": op.v}, Replay: store.NewReplay(store.ReplayOptions{}), Tracer: tracer,
		Poster: posterFunc(func(ctx context.Context, _ string, msg []byte) ([]byte, error) {
			return refusing.Process(ctx, msg).Response, nil
		}),
	})
	if _, err := asker.Exchange(context.Background(), envelope.NewHeader(intents.BookingRequest, shipperDomain, operatorDomain, time.Now()),
		bookingRequest(), intents.BookingConfirmation); err == nil {
		t.Fatal("no refusal")
	}
	eg = find(exp.GetSpans(), "ulp.egress")
	if eg.Status.Description != "forbidden" || attr(eg, "ulp.code") != "forbidden" {
		t.Errorf("refused egress %+v %+v", eg.Status, eg.Attributes)
	}
	noBodies(t, exp.GetSpans(), "Mallory says hello")
}

// TestTraceRemoteContext: a partner's trace context is linked to, not
// continued, unless the partner is trusted; a message that doesn't parse
// is never continued.
func TestTraceRemoteContext(t *testing.T) {
	op, cy := newParty(t, "ob-2026-1"), newParty(t, "cy-2026-1")
	keys := staticKeys{shipperDomain + "/cy-2026-1": cy.v}
	remote := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1, 2, 3}, SpanID: trace.SpanID{4, 5, 6},
		TraceFlags: trace.FlagsSampled, Remote: true})
	h := envelope.NewHeader(intents.BookingRequest, shipperDomain, operatorDomain, time.Now())
	b, _ := json.Marshal(bookingRequest())
	for name, c := range map[string]struct {
		trust     func(string) bool
		msg       []byte
		continued bool
	}{
		"untrusted":        {nil, nil, false},
		"another trusted":  {func(d string) bool { return d == "mallory.example" }, nil, false},
		"trusted":          {func(d string) bool { return d == shipperDomain }, nil, true},
		"trusted, garbled": {func(string) bool { return true }, []byte("not a JWS"), false},
	} {
		tracer, exp := recorder()
		cfg := baseConfig(t, op, keys, handlerFunc{handle: func(context.Context, *node.Request) (*node.Reply, error) { return nil, nil }})
		cfg.Tracer, cfg.TrustTrace = tracer, c.trust
		n, err := node.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		msg := c.msg
		if msg == nil {
			h.MessageID = envelope.NewHeader(intents.BookingRequest, shipperDomain, operatorDomain, time.Now()).MessageID
			msg, _ = envelope.Seal(&envelope.Envelope{Header: h, Body: b}, "cy-2026-1", cy.signer)
		}
		n.Process(node.WithRemoteTrace(context.Background(), remote), msg)
		root := find(exp.GetSpans(), "ulp.ingress")
		same := root.SpanContext.TraceID() == remote.TraceID() && root.Parent.SpanID() == remote.SpanID()
		linked := len(root.Links) == 1 && root.Links[0].SpanContext.TraceID() == remote.TraceID()
		if same != c.continued || linked == c.continued {
			t.Errorf("%s: continued %v, linked %v", name, same, linked)
		}
		if parse := find(exp.GetSpans(), "ulp.parse"); parse.Parent.SpanID() != root.SpanContext.SpanID() || parse.StartTime.After(root.StartTime.Add(time.Millisecond)) {
			t.Errorf("%s: parse span %+v", name, parse.Parent)
		}
	}
	if node.RemoteTrace(node.WithRemoteTrace(context.Background(), trace.SpanContext{})).IsValid() {
		t.Error("an invalid context was kept")
	}
}

// TestTraceRemoteSampling: a partner's sampled flag doesn't make us record
// (the sampler only follows our own parents), and its tracestate never
// reaches our spans; a trusted claimed sender is continued even when its
// signature fails (the documented trade-off: the trace starts first).
func TestTraceRemoteSampling(t *testing.T) {
	op, cy := newParty(t, "ob-2026-1"), newParty(t, "cy-2026-1")
	mallory := newParty(t, "cy-2026-1")
	keys := staticKeys{shipperDomain + "/cy-2026-1": cy.v}
	ts, _ := trace.ParseTraceState("vendor=padding")
	remote := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2},
		TraceFlags: trace.FlagsSampled, TraceState: ts, Remote: true})
	run := func(sampler sdktrace.Sampler, trust func(string) bool, signer party) tracetest.SpanStubs {
		exp := tracetest.NewInMemoryExporter()
		tracer := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp), sdktrace.WithSampler(sampler)).Tracer("t")
		cfg := baseConfig(t, op, keys, handlerFunc{handle: func(context.Context, *node.Request) (*node.Reply, error) { return nil, nil }})
		cfg.Tracer, cfg.TrustTrace = tracer, trust
		n, _ := node.New(cfg)
		h := envelope.NewHeader(intents.BookingRequest, shipperDomain, operatorDomain, time.Now())
		b, _ := json.Marshal(bookingRequest())
		msg, _ := envelope.Seal(&envelope.Envelope{Header: h, Body: b}, "cy-2026-1", signer.signer)
		n.Process(node.WithRemoteTrace(context.Background(), remote), msg)
		return exp.GetSpans()
	}
	never := sdktrace.ParentBased(sdktrace.NeverSample(), sdktrace.WithRemoteParentSampled(sdktrace.NeverSample()))
	if spans := run(never, nil, cy); len(spans) != 0 {
		t.Errorf("an untrusted sampled flag recorded %d spans", len(spans))
	}
	if spans := run(never, func(string) bool { return true }, cy); len(spans) != 0 {
		t.Errorf("a trusted sampled flag recorded %d spans", len(spans))
	}
	spans := run(sdktrace.AlwaysSample(), nil, cy)
	for _, s := range spans {
		for _, l := range s.Links {
			if l.SpanContext.TraceState().Len() != 0 {
				t.Error("the partner's tracestate is in our link")
			}
		}
	}
	spans = run(sdktrace.AlwaysSample(), func(d string) bool { return d == shipperDomain }, mallory)
	root := find(spans, "ulp.ingress")
	if root.SpanContext.TraceID() != remote.TraceID() || root.SpanContext.TraceState().Len() != 0 || attr(root, "ulp.code") != "bad_signature" {
		t.Errorf("trusted claim, bad signature: %v %v", root.SpanContext, root.Attributes)
	}
}
