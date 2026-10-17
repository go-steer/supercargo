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

package shipper

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/intents"
)

// Inbound handles the pushes an operator sends for a booking:
// ulp.tnt.event and ulp.tnt.alert (docs/DESIGN.md, A.9).
type Inbound struct {
	Bookings *store.Bookings
	Log      *store.EventLog
	Now      func() time.Time
	// OnAlert runs for each new alert, after it is recorded: the hook the
	// alert summarizer attaches to. Optional. It runs inside the push's
	// HTTP request, so it must return quickly: slow work (a model call)
	// belongs in its own goroutine, or the operator times out and re-pushes.
	// Cause event IDs in the alert are the operator's claims, unchecked
	// against the log; check them before they reach a model.
	OnAlert func(b store.Booking, a *intents.TnTAlertBody)
	// OnEvent runs for each new event, after it is recorded (the node's
	// log shows it). Optional; the same rules as OnAlert.
	OnEvent func(b store.Booking, e *intents.TnTEventBody)
}

// forbidden is the one refusal for every reason, so the answer says
// nothing about which bookings this shipper has.
var forbidden = node.Reject(intents.CodeForbidden, "The sender may not push to this booking.", "")

// Authorize implements node.Handler (docs/DESIGN.md, A.6): the sender must
// be the booking's operator, and the booking must have an active
// subscription that asks for this intent.
func (in *Inbound) Authorize(_ context.Context, req *node.Request) error {
	h := req.Envelope.Header
	// Bookings are keyed by (operator, ref): finding one under the sender
	// is what proves the sender is its operator.
	b, err := in.Bookings.Get(h.Sender, *h.BookingRef)
	if err != nil {
		return &node.Rejection{Code: forbidden.Code, Detail: forbidden.Detail, Reason: "no booking " + *h.BookingRef + " with operator " + h.Sender}
	}
	sub := b.Subscription()
	if sub == nil || !slices.Contains(sub.Intents, h.Intent) {
		return &node.Rejection{Code: forbidden.Code, Detail: forbidden.Detail, Reason: "booking " + b.Ref + " has no subscription to " + h.Intent}
	}
	if b.Delegated != nil && !b.Delegated.Active(in.Now()) {
		// Its subscription ended with the grant.
		return &node.Rejection{Code: forbidden.Code, Detail: forbidden.Detail, Reason: "the delegation for " + b.Ref + " has expired"}
	}
	if b.Cancelled() {
		// Its subscription ended with it.
		return &node.Rejection{Code: forbidden.Code, Detail: forbidden.Detail, Reason: "booking " + b.Ref + " was cancelled"}
	}
	return nil
}

// Handle implements node.Handler: record the push (once per event or alert
// ID), and hand a new alert to OnAlert. A push has no reply.
func (in *Inbound) Handle(_ context.Context, req *node.Request) (*node.Reply, error) {
	h := req.Envelope.Header
	var id string
	var alert *intents.TnTAlertBody
	var event *intents.TnTEventBody
	switch b := req.Body.(type) {
	case *intents.TnTEventBody:
		id, event = b.EventID, b
	case *intents.TnTAlertBody:
		id, alert = b.AlertID, b
	default:
		return nil, errors.New("shipper: inbound handler got a " + req.Body.Intent())
	}
	added, err := in.Log.Add(h.Sender, *h.BookingRef, store.Record{
		ID: id, Intent: h.Intent, Header: h, Body: req.Envelope.Body, At: in.Now(),
	})
	if err != nil {
		return nil, err
	}
	if !added || (alert == nil || in.OnAlert == nil) && (event == nil || in.OnEvent == nil) {
		return nil, nil
	}
	// Found by Authorize a moment ago, and bookings are never removed.
	b, _ := in.Bookings.Get(h.Sender, *h.BookingRef)
	if alert != nil {
		in.OnAlert(b, alert)
	} else {
		in.OnEvent(b, event)
	}
	return nil, nil
}
