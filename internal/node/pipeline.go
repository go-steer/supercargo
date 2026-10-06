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

	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/discovery"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
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
	u, err := envelope.Parse(msg)
	if err != nil {
		n.cfg.Observer(Event{Time: n.cfg.Now(), Direction: "in", Outcome: "malformed", Reason: err.Error()})
		return rest.Result{Status: http.StatusBadRequest}
	}
	claimed := u.ClaimedHeader()
	ev := Event{Direction: "in", Intent: claimed.Intent, Sender: claimed.Sender, Receiver: claimed.Receiver,
		MessageID: claimed.MessageID, KID: u.KID()}

	env, status, rej := n.verify(ctx, u, &ev)
	if env == nil {
		n.observeRejection(ev, rej)
		return rest.Result{Status: status}
	}

	// Verified from here on: h is the signed header, sent by a party whose
	// key resolved, addressed to this node.
	h := env.Header
	sideEffects := false
	defer func() {
		if p := recover(); p != nil {
			res = n.refuse(&h, ev, &Rejection{Code: intents.CodeInternal, Retryable: !sideEffects,
				Reason: "panic: " + bounded(fmt.Sprint(p))})
		}
	}()
	body, rej := n.admit(env)
	if rej != nil {
		return n.refuse(&h, ev, rej)
	}
	req := &Request{Envelope: env, Body: body}
	handler := n.cfg.Handlers[h.Intent]
	if err := handler.Authorize(ctx, req); err != nil {
		return n.refuse(&h, ev, asRejection(err, intents.CodeForbidden, true))
	}
	// A handler may change state before it fails. Its failures are then
	// not retryable: re-sending could do the work twice.
	sideEffects = true
	reply, err := handler.Handle(ctx, req)
	if err != nil {
		return n.refuse(&h, ev, asRejection(err, intents.CodeInternal, false))
	}
	ev.Outcome, ev.Time = "accepted", n.cfg.Now()
	n.cfg.Observer(ev)
	if reply == nil || reply.Body == nil {
		return rest.Result{}
	}
	out, err := n.sealReply(&h, reply)
	if err != nil {
		ev.Outcome, ev.Reason, ev.Time = "failed", "sealing the reply: "+err.Error(), n.cfg.Now()
		n.cfg.Observer(ev)
		return rest.Result{Status: http.StatusInternalServerError}
	}
	return rest.Result{Response: out}
}

// verify runs the steps before and including signature verification. On
// failure it returns the bare HTTP status to answer with, and why.
func (n *Node) verify(ctx context.Context, u *envelope.Unverified, ev *Event) (*envelope.Envelope, int, *Rejection) {
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
	v, err := n.cfg.Keys.Key(ctx, u.Sender(), u.KID())
	if err != nil {
		status := http.StatusForbidden
		if errors.Is(err, discovery.ErrUnavailable) {
			status = http.StatusServiceUnavailable
		}
		return nil, status, &Rejection{Code: intents.CodeUnknownKey, Reason: err.Error()}
	}
	env, err := u.Verify(v)
	if err != nil {
		return nil, http.StatusForbidden, &Rejection{Code: intents.CodeBadSignature, Reason: err.Error()}
	}
	ev.Verified = true
	return env, 0, nil
}

// admit runs the checks on a verified envelope that decide whether it may
// reach its handler: freshness, replay, and the body.
func (n *Node) admit(env *envelope.Envelope) (intents.Body, *Rejection) {
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
	body, err := intents.Decode(env)
	if err != nil {
		code := intents.CodeInvalidBody
		if errors.Is(err, intents.ErrUnsupportedIntent) {
			code = intents.CodeUnsupportedIntent
		}
		return nil, &Rejection{Code: code, Reason: err.Error()}
	}
	return body, nil
}

func (n *Node) observeRejection(ev Event, rej *Rejection) {
	ev.Outcome, ev.Code, ev.Reason, ev.Time = "rejected", rej.Code, rej.Reason, n.cfg.Now()
	n.cfg.Observer(ev)
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
func (n *Node) refuse(h *envelope.Header, ev Event, rej *Rejection) rest.Result {
	n.observeRejection(ev, rej)
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
	out, err := n.sealReply(h, &Reply{Body: body})
	if err != nil {
		return rest.Result{Status: http.StatusInternalServerError}
	}
	return rest.Result{Response: out}
}

// sealReply signs reply as a response to the verified header h.
func (n *Node) sealReply(h *envelope.Header, reply *Reply) ([]byte, error) {
	if err := reply.Body.Validate(); err != nil {
		return nil, fmt.Errorf("node: invalid %s reply body: %w", reply.Body.Intent(), err)
	}
	b, err := json.Marshal(reply.Body)
	if err != nil {
		return nil, err
	}
	rh := h.Reply(reply.Body.Intent(), n.cfg.Now())
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
	out, err := envelope.Seal(env, n.cfg.KID, n.cfg.Signer)
	if err != nil {
		return nil, err
	}
	if len(out) > envelope.MaxSize {
		return nil, fmt.Errorf("node: reply is %d bytes, over the %d the receiver accepts", len(out), envelope.MaxSize)
	}
	n.cfg.Observer(Event{Time: n.cfg.Now(), Direction: "out", Intent: rh.Intent, Sender: rh.Sender, Receiver: rh.Receiver,
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
