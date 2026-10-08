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
	"fmt"
	"log/slog"
	"time"

	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
)

// Request is an inbound envelope that has passed every pipeline step up to
// authorization: verified, fresh, not a replay, addressed here, with a
// valid body.
type Request struct {
	Envelope *envelope.Envelope
	Body     intents.Body
}

// Reply is a handler's answer. A nil Body means no response (an accepted
// push). BookingRef sets the response's header bookingRef; when empty, the
// request's is copied.
type Reply struct {
	Body       intents.Body
	BookingRef string
}

// Handler serves one intent. The pipeline calls Authorize, and Handle only
// if Authorize returned nil: authorization happens before any data the
// request is after is loaded, and before any model is involved. Authorize
// should load no more than the decision needs (the booking's parties, say).
type Handler interface {
	Authorize(ctx context.Context, req *Request) error
	Handle(ctx context.Context, req *Request) (*Reply, error)
}

// Rejection is a refusal with a ulp.error code. Detail is sent to the
// sender, so it must be a fixed, safe sentence, never internal error text;
// Reason is for this node's log only.
type Rejection struct {
	Code      string
	Detail    string
	Retryable bool
	Reason    string
}

func (r *Rejection) Error() string {
	if r.Reason != "" {
		return fmt.Sprintf("%s: %s (%s)", r.Code, r.Detail, r.Reason)
	}
	return r.Code + ": " + r.Detail
}

// Reject returns a non-retryable Rejection.
func Reject(code, detail, reason string) *Rejection {
	return &Rejection{Code: code, Detail: detail, Reason: reason}
}

// Fixed details for the pipeline's own rejections. None carries anything
// from the request, so nothing an attacker sends is echoed back signed.
var details = map[string]string{
	intents.CodeBadSignature:      "The signature does not verify with the sender's published key.",
	intents.CodeUnknownKey:        "The sender's signing key could not be resolved.",
	intents.CodeStale:             "sentAt is outside the allowed clock skew.",
	intents.CodeReplay:            "This messageId has already been received.",
	intents.CodeInvalidBody:       "The body or header does not follow the intent's rules.",
	intents.CodeUnsupportedIntent: "This node does not accept this intent.",
	intents.CodeForbidden:         "The request is not allowed.",
	intents.CodeNotFound:          "No such booking.",
	intents.CodeInternal:          "The request could not be processed.",
}

// Event is one decision the node made, as structured data: the log line is
// rendered from it, and anything else (a UI, metrics) can consume it too.
type Event struct {
	Time      time.Time
	Direction string // "in" or "out"
	Intent    string
	Sender    string
	Receiver  string
	MessageID string
	KID       string
	// Outcome is "accepted", "rejected" or "malformed" for inbound
	// messages, and "sent", "response" or "failed" for outbound ones.
	Outcome  string
	Verified bool
	Code     string // the ulp.error code, when rejected
	Reason   string // why, for this node's log; never sent
	// Via is the binding a request came or went over: "rest" or "a2a".
	Via string

	// The message itself, for the node's message log (never the slog log):
	// Wire is its exact bytes as sent or received (nil if there were
	// none); Header is its header, the signed one once Verified and as
	// claimed before that (nil if it didn't parse); Body is its body, the
	// verified bytes inbound or what this node sent (nil otherwise).
	Wire   []byte
	Header *envelope.Header
	Body   json.RawMessage
	// Opener is the intent that opened this message's conversation (the
	// request a reply answers), and OpenedHere whether this node sent it:
	// together they say which of a node's roles the message belongs to.
	Opener     string
	OpenedHere bool
}

// Observer receives events. It must not block.
type Observer func(Event)

// SlogObserver logs each event as one structured record.
func SlogObserver(l *slog.Logger) Observer {
	return func(e Event) {
		level := slog.LevelInfo
		if e.Outcome == "rejected" || e.Outcome == "malformed" || e.Outcome == "failed" {
			level = slog.LevelWarn
		}
		l.LogAttrs(context.Background(), level, "envelope",
			slog.String("dir", e.Direction),
			slog.String("intent", e.Intent),
			slog.String("sender", e.Sender),
			slog.String("receiver", e.Receiver),
			slog.String("via", e.Via),
			slog.String("messageId", e.MessageID),
			slog.String("kid", e.KID),
			slog.Bool("verified", e.Verified),
			slog.String("outcome", e.Outcome),
			slog.String("code", e.Code),
			slog.String("reason", e.Reason),
		)
	}
}
