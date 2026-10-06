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
	"slices"
	"time"

	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
	"github.com/go-steer/supercargo/pkg/transport/a2a"
	"github.com/go-steer/supercargo/pkg/transport/rest"
)

// RemoteError is a ulp.error a partner answered with.
type RemoteError struct {
	Code      string
	Detail    string
	Retryable bool
}

func (e *RemoteError) Error() string {
	return fmt.Sprintf("node: partner refused: %s: %s", e.Code, e.Detail)
}

// ErrBadResponse reports a response that failed the node's checks. The
// exchange's outcome is unknown: the request may or may not have been
// acted on.
var ErrBadResponse = errors.New("node: response rejected")

// Exchange signs body as a new message (or, with inReplyTo set on h, a
// reply), sends it to h.Receiver, and returns the partner's verified,
// decoded response, which must have one of the expect intents. A partner's
// ulp.error comes back as a *RemoteError. With no expect intents, a 204 is
// the only acceptable answer and Exchange returns nil, nil.
//
// The response gets the same checks an inbound envelope does, plus that it
// answers this message: from h.Receiver, to this node, inReplyTo this
// messageId, in the same conversation.
func (n *Node) Exchange(ctx context.Context, h envelope.Header, body intents.Body, expect ...string) (*Request, error) {
	if n.cfg.Poster == nil {
		return nil, errors.New("node: no Poster configured")
	}
	return n.exchange(ctx, h, body, expect, func(msg []byte) ([]byte, string, error) {
		resp, err := n.cfg.Poster.Post(ctx, h.Receiver, msg)
		return resp, "rest", err
	})
}

// ExchangeA2A is Exchange over the A2A binding, for the agentic intents
// (an inquiry): the receiver's agent takes the task. A receiver that
// publishes no A2A endpoint gets the same message over REST, so the
// binding never decides whether a message can be sent.
func (n *Node) ExchangeA2A(ctx context.Context, h envelope.Header, body intents.Body, expect ...string) (*Request, error) {
	if n.cfg.A2A == nil {
		return n.Exchange(ctx, h, body, expect...)
	}
	if n.cfg.Poster == nil {
		return nil, errors.New("node: no Poster configured")
	}
	return n.exchange(ctx, h, body, expect, func(msg []byte) ([]byte, string, error) {
		resp, err := n.cfg.A2A.Post(ctx, h.Receiver, msg)
		if !errors.Is(err, a2a.ErrNoEndpoint) {
			return resp, "a2a", err
		}
		resp, err = n.cfg.Poster.Post(ctx, h.Receiver, msg)
		return resp, "rest", err
	})
}

// Push signs body as a new message to h.Receiver and posts it to endpoint,
// a webhook the receiver registered (it must be on the receiver's domain).
// A push expects no response: nil means the receiver accepted it (204). A
// signed ulp.error comes back as a *RemoteError, a bare refusal as a
// *rest.StatusError.
func (n *Node) Push(ctx context.Context, h envelope.Header, body intents.Body, endpoint string) error {
	if n.cfg.Poster == nil {
		return errors.New("node: no Poster configured")
	}
	_, err := n.exchange(ctx, h, body, nil, func(msg []byte) ([]byte, string, error) {
		resp, err := n.cfg.Poster.PostTo(ctx, h.Receiver, endpoint, msg)
		return resp, "rest", err
	})
	return err
}

func (n *Node) exchange(ctx context.Context, h envelope.Header, body intents.Body, expect []string, post func([]byte) ([]byte, string, error)) (*Request, error) {
	if h.Sender != n.cfg.Domain || h.Intent != body.Intent() {
		return nil, fmt.Errorf("node: header sender %q / intent %q don't match this node and the body", h.Sender, h.Intent)
	}
	if err := body.Validate(); err != nil {
		return nil, fmt.Errorf("node: invalid %s body: %w", body.Intent(), err)
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	out := &envelope.Envelope{Header: h, Body: b}
	msg, err := envelope.Seal(out, n.cfg.KID, n.cfg.Signer)
	if err != nil {
		return nil, err
	}
	ev := Event{Direction: "out", Intent: h.Intent, Sender: h.Sender, Receiver: h.Receiver, MessageID: h.MessageID, KID: n.cfg.KID, Verified: true}
	resp, via, err := post(msg)
	ev.Via = via
	if errors.Is(err, rest.ErrNoResponse) && len(expect) == 0 {
		ev.Outcome, ev.Time = "sent", n.cfg.Now()
		n.cfg.Observer(ev)
		return nil, nil
	}
	if err != nil {
		ev.Outcome, ev.Reason, ev.Time = "failed", err.Error(), n.cfg.Now()
		n.cfg.Observer(ev)
		return nil, err
	}
	ev.Outcome, ev.Time = "sent", n.cfg.Now()
	n.cfg.Observer(ev)
	return n.checkResponse(ctx, h, resp, expect, via)
}

func (n *Node) checkResponse(ctx context.Context, req envelope.Header, resp []byte, expect []string, via string) (*Request, error) {
	ev := Event{Direction: "in", Via: via, Outcome: "rejected"}
	fail := func(reason string) (*Request, error) {
		ev.Reason, ev.Time = reason, n.cfg.Now()
		n.cfg.Observer(ev)
		return nil, fmt.Errorf("%w: %s", ErrBadResponse, reason)
	}
	u, err := envelope.Parse(resp)
	if err != nil {
		return fail(err.Error())
	}
	h := u.ClaimedHeader()
	ev.Intent, ev.Sender, ev.Receiver, ev.MessageID, ev.KID = h.Intent, h.Sender, h.Receiver, h.MessageID, u.KID()
	switch {
	case h.Sender != req.Receiver:
		return fail("response is from " + h.Sender + ", not " + req.Receiver)
	case h.Receiver != n.cfg.Domain:
		return fail("response is addressed to " + h.Receiver)
	case h.InReplyTo == nil || *h.InReplyTo != req.MessageID:
		return fail("response does not answer this message")
	case h.CorrelationID != req.CorrelationID:
		return fail("response is from another conversation")
	case h.Intent != intents.Error && !slices.Contains(expect, h.Intent):
		return fail("unexpected response intent " + h.Intent)
	}
	v, err := n.cfg.Keys.Key(ctx, u.Sender(), u.KID())
	if err != nil {
		return fail(err.Error())
	}
	env, err := u.Verify(v)
	if err != nil {
		return fail(err.Error())
	}
	ev.Verified = true
	sent, err := env.Header.SentTime()
	if err != nil {
		return fail(err.Error())
	}
	if skew := n.cfg.Now().Sub(sent); skew > n.cfg.MaxSkew || skew < -n.cfg.MaxSkew {
		return fail(fmt.Sprintf("response sentAt is %v from now", skew.Round(time.Second)))
	}
	body, err := intents.Decode(env)
	if err != nil {
		return fail(err.Error())
	}
	ev.Outcome, ev.Time = "response", n.cfg.Now()
	n.cfg.Observer(ev)
	if e, ok := body.(*intents.ErrorBody); ok {
		return &Request{Envelope: env, Body: body}, &RemoteError{Code: e.Code, Detail: e.Detail, Retryable: e.Retryable}
	}
	return &Request{Envelope: env, Body: body}, nil
}
