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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/discovery"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
	"github.com/go-steer/supercargo/pkg/transport/a2a"
	"github.com/go-steer/supercargo/pkg/transport/rest"
)

// Process runs one inbound serialized envelope through the pipeline and
// returns what the transport should answer. It implements rest.Processor.
//
// Until the sender is verified, every refusal is a bare HTTP status: the
// node signs nothing in answer to a message it hasn't verified, so nobody
// can obtain a signed statement (with a correlationId, bookingRef or
// receiver of their choosing) without holding a key the node trusts. Once
// verified, refusals are signed ulp.error replies.
func (n *Node) Process(ctx context.Context, msg []byte) (res rest.Result) {
	// The trace starts once the claimed sender is known, which decides
	// whether a partner's trace context may be our parent (ingressTrace);
	// the parse is recorded from the start.
	start := time.Now()
	u, err := envelope.Parse(msg)
	parsed := time.Now()
	var sender string
	if err == nil {
		sender = u.ClaimedHeader().Sender
	}
	ctx, span := n.ingressTrace(ctx, sender, start)
	defer span.End()
	defer func() { n.recordDuration(ctx, start, u, res) }()
	_, ps := n.start(ctx, "ulp.parse", trace.WithTimestamp(start))
	if err != nil {
		ps.SetStatus(codes.Error, bounded(err.Error()))
	}
	ps.End(trace.WithTimestamp(parsed))
	if err != nil {
		n.observe(ctx, Event{Time: n.cfg.Now(), Direction: "in", Outcome: "malformed", Reason: err.Error()})
		return rest.Result{Status: http.StatusBadRequest}
	}
	claimed := u.ClaimedHeader()
	via := "rest"
	if a2a.Via(ctx) {
		via = "a2a"
	}
	ev := Event{Direction: "in", Via: via, Intent: claimed.Intent, Sender: claimed.Sender, Receiver: claimed.Receiver,
		MessageID: claimed.MessageID, KID: u.KID()}

	env, status, rej := n.verify(ctx, u, &ev)
	if env == nil {
		n.observeRejection(ctx, ev, rej)
		return rest.Result{Status: status}
	}

	// Verified from here on: h is the signed header, sent by a party whose
	// key resolved, addressed to this node.
	h := env.Header
	sideEffects := false
	defer func() {
		if p := recover(); p != nil {
			res = n.refuse(ctx, &h, ev, &Rejection{Code: intents.CodeInternal, Retryable: !sideEffects,
				Reason: "panic: " + bounded(fmt.Sprint(p))})
		}
	}()
	body, rej := n.admit(ctx, env)
	if rej != nil {
		return n.refuse(ctx, &h, ev, rej)
	}
	req := &Request{Envelope: env, Body: body}
	handler := n.cfg.Handlers[h.Intent]
	if rej := n.authorize(ctx, handler, req); rej != nil {
		return n.refuse(ctx, &h, ev, rej)
	}
	// A handler may change state before it fails. Its failures are then
	// not retryable: re-sending could do the work twice.
	sideEffects = true
	reply, err := n.handle(ctx, handler, req)
	if err != nil {
		return n.refuse(ctx, &h, ev, asRejection(err, intents.CodeInternal, false))
	}
	ev.Outcome, ev.Time = "accepted", n.cfg.Now()
	n.observe(ctx, ev)
	if reply == nil || reply.Body == nil {
		return rest.Result{}
	}
	out, err := n.sealReply(ctx, &h, reply, ev.Via)
	if err != nil {
		ev.Outcome, ev.Reason, ev.Time = "failed", "sealing the reply: "+err.Error(), n.cfg.Now()
		n.observe(ctx, ev)
		return rest.Result{Status: http.StatusInternalServerError}
	}
	return rest.Result{Response: out}
}

// authorize runs the handler's authorization in its span, which ends even
// if the handler panics (as a failure; Process's recover answers).
func (n *Node) authorize(ctx context.Context, handler Handler, req *Request) (rej *Rejection) {
	ctx, span := n.start(ctx, "ulp.authorize")
	panicked := true
	defer func() {
		switch {
		case panicked:
			span.SetStatus(codes.Error, "panic")
		case rej != nil:
			span.SetAttributes(AttrDecision.String("denied"), AttrCode.String(rej.Code))
			span.SetStatus(codes.Error, rej.Code)
		default:
			span.SetAttributes(AttrDecision.String("allowed"))
		}
		span.End()
	}()
	err := handler.Authorize(ctx, req)
	panicked = false
	if err != nil {
		return asRejection(err, intents.CodeForbidden, true)
	}
	return nil
}

// handle runs the handler in its span, which ends even if it panics.
func (n *Node) handle(ctx context.Context, handler Handler, req *Request) (reply *Reply, err error) {
	ctx, span := n.start(ctx, "ulp.handle")
	panicked := true
	defer func() {
		if panicked {
			span.SetStatus(codes.Error, "panic")
		}
		fail(span, err)
	}()
	reply, err = handler.Handle(ctx, req)
	panicked = false
	return reply, err
}

// verify runs the steps before and including signature verification. On
// failure it returns the bare HTTP status to answer with, and why.
func (n *Node) verify(ctx context.Context, u *envelope.Unverified, ev *Event) (env *envelope.Envelope, status int, rej *Rejection) {
	ctx, span := n.start(ctx, "ulp.verify")
	defer func() {
		if rej != nil {
			span.SetStatus(codes.Error, rej.Code)
		}
		span.End()
	}()
	claimed := u.ClaimedHeader()
	// Addressed here, and an intent served here? Checked on the claimed
	// header, before any key is fetched; the claim is the signed value once
	// verified, so checking early only saves the fetch.
	if claimed.Receiver != n.cfg.Domain {
		return nil, http.StatusMisdirectedRequest, &Rejection{Code: intents.CodeForbidden, Reason: "receiver is " + claimed.Receiver}
	}
	if _, ok := n.cfg.Handlers[claimed.Intent]; !ok {
		return nil, http.StatusBadRequest, &Rejection{Code: intents.CodeUnsupportedIntent, Reason: "no handler for " + claimed.Intent}
	}
	kctx, ks := n.start(ctx, "ulp.key.resolve")
	v, err := n.cfg.Keys.Key(kctx, u.Sender(), u.KID())
	fail(ks, err)
	if err != nil {
		status := http.StatusForbidden
		if errors.Is(err, discovery.ErrUnavailable) {
			status = http.StatusServiceUnavailable
		}
		return nil, status, &Rejection{Code: intents.CodeUnknownKey, Reason: err.Error()}
	}
	env, err = u.Verify(v)
	if err != nil {
		return nil, http.StatusForbidden, &Rejection{Code: intents.CodeBadSignature, Reason: err.Error()}
	}
	ev.Verified = true
	span.SetAttributes(AttrVerified.Bool(true), AttrKID.String(u.KID()))
	return env, 0, nil
}

// admit runs the checks on a verified envelope that decide whether it may
// reach its handler: freshness, replay, and the body.
func (n *Node) admit(ctx context.Context, env *envelope.Envelope) (body intents.Body, rej *Rejection) {
	_, span := n.start(ctx, "ulp.replay")
	defer func() {
		if rej != nil {
			span.SetStatus(codes.Error, rej.Code)
		}
		span.End()
	}()
	sent, err := env.Header.SentTime()
	if err != nil {
		return nil, &Rejection{Code: intents.CodeInvalidBody, Reason: err.Error()}
	}
	if skew := n.cfg.Now().Sub(sent); skew > n.cfg.MaxSkew || skew < -n.cfg.MaxSkew {
		return nil, &Rejection{Code: intents.CodeStale, Reason: fmt.Sprintf("sentAt is %v from now", skew.Round(time.Second))}
	}
	switch err := n.cfg.Replay.Record(env.Header.Sender, env.Header.MessageID); {
	case errors.Is(err, store.ErrReplay):
		return nil, &Rejection{Code: intents.CodeReplay, Reason: "messageId already seen"}
	case err != nil:
		return nil, &Rejection{Code: intents.CodeInternal, Retryable: true, Reason: err.Error()}
	}
	span.End()
	_, span = n.start(ctx, "ulp.decode")
	body, err = intents.Decode(env)
	if err != nil {
		code := intents.CodeInvalidBody
		if errors.Is(err, intents.ErrUnsupportedIntent) {
			code = intents.CodeUnsupportedIntent
		}
		return nil, &Rejection{Code: code, Reason: err.Error()}
	}
	return body, nil
}

func (n *Node) observeRejection(ctx context.Context, ev Event, rej *Rejection) {
	ev.Outcome, ev.Code, ev.Reason, ev.Time = "rejected", rej.Code, rej.Reason, n.cfg.Now()
	n.observe(ctx, ev)
}

// asRejection turns a handler's error into a Rejection: a *Rejection as
// is, anything else as code with a fixed detail (its text stays in the log).
func asRejection(err error, code string, retryable bool) *Rejection {
	var r *Rejection
	if errors.As(err, &r) {
		return r
	}
	return &Rejection{Code: code, Retryable: retryable && code == intents.CodeInternal, Reason: err.Error()}
}

// refuse logs a rejection of a verified envelope and answers it with a
// signed ulp.error.
func (n *Node) refuse(ctx context.Context, h *envelope.Header, ev Event, rej *Rejection) rest.Result {
	n.observeRejection(ctx, ev, rej)
	detail := rej.Detail
	if detail == "" {
		detail = details[rej.Code]
	}
	body := &intents.ErrorBody{Code: rej.Code, Detail: detail, Retryable: rej.Retryable}
	if body.Validate() != nil {
		// A handler's detail that isn't safe text is replaced, not sent.
		body.Detail = details[rej.Code]
		if body.Validate() != nil {
			body = &intents.ErrorBody{Code: intents.CodeInternal, Detail: details[intents.CodeInternal]}
		}
	}
	out, err := n.sealReply(ctx, h, &Reply{Body: body}, ev.Via)
	if err != nil {
		return rest.Result{Status: http.StatusInternalServerError}
	}
	return rest.Result{Response: out}
}

// sealReply signs reply as a response to the verified header h.
func (n *Node) sealReply(ctx context.Context, h *envelope.Header, reply *Reply, via string) (out []byte, err error) {
	ctx, span := n.start(ctx, "ulp.reply.seal")
	defer func() { fail(span, err) }()
	if err := reply.Body.Validate(); err != nil {
		return nil, fmt.Errorf("node: invalid %s reply body: %w", reply.Body.Intent(), err)
	}
	b, err := json.Marshal(reply.Body)
	if err != nil {
		return nil, err
	}
	// Replies are outbound too: the same guard as requests (fail closed:
	// the sender gets a bare 500, nothing signed).
	if n.cfg.OutboundGuard != nil {
		if err := n.cfg.OutboundGuard(b); err != nil {
			return nil, fmt.Errorf("node: refused to reply %s: %w", reply.Body.Intent(), err)
		}
	}
	rh := h.Reply(reply.Body.Intent(), n.cfg.Now())
	intents.SetDialect(&rh)
	if rh.Sender != n.cfg.Domain {
		// Unreachable through Process, which only replies to envelopes
		// addressed here; the node never signs for another domain.
		return nil, fmt.Errorf("node: refusing to sign a reply from %s", rh.Sender)
	}
	if reply.BookingRef != "" {
		ref := reply.BookingRef
		rh.BookingRef = &ref
	}
	env := &envelope.Envelope{Header: rh, Body: b}
	out, err = envelope.Seal(env, n.cfg.KID, n.cfg.Signer)
	if err != nil {
		return nil, err
	}
	if len(out) > envelope.MaxSize {
		return nil, fmt.Errorf("node: reply is %d bytes, over the %d the receiver accepts", len(out), envelope.MaxSize)
	}
	n.observe(ctx, Event{Time: n.cfg.Now(), Direction: "out", Via: via, Intent: rh.Intent, Sender: rh.Sender, Receiver: rh.Receiver,
		MessageID: rh.MessageID, KID: n.cfg.KID, Outcome: "sent", Verified: true})
	return out, nil
}

// bounded cuts s to 256 bytes on a rune boundary, for log fields that
// carry values nobody bounded (a panic's).
func bounded(s string) string {
	const limit = 256
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}
