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

package forwarder

import (
	"context"

	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/intents"
)

// Status answers ulp.forwarding.status.request: where an order stands, as
// the forwarder holds it, for the shipper that placed it. It is
// deterministic, no model involved, and lets a shipper whose first answer
// was lost find its order (docs/DESIGN.md, A.5).
type Status struct {
	Orders *Orders
	// Log holds the orders' events and alerts, as the carrier side
	// published them: the delivery's estimate comes from them.
	Log *store.EventLog
}

// Authorize implements node.Handler (docs/DESIGN.md, A.6): anyone may ask,
// and only about its own orders. The lookup is under the sender, so
// another party's order is never found: it gets what a reference with no
// order gets, nothing.
func (s *Status) Authorize(context.Context, *node.Request) error { return nil }

// Handle implements node.Handler: the sender's latest order under the
// reference it gave, if any.
func (s *Status) Handle(_ context.Context, req *node.Request) (*node.Reply, error) {
	h := req.Envelope.Header
	ref := req.Body.(*intents.ForwardingStatusRequestBody).ShipperRef
	out := &intents.ForwardingStatusResponseBody{ShipperRef: ref}
	self := s.Orders.Node.Domain()
	var found *store.Booking
	for _, b := range s.Orders.Bookings.List() {
		if b.Order == nil || b.Operator != self || b.Shipper != h.Sender || b.Order.Request.ShipperRef != ref {
			continue
		}
		if found == nil || b.CreatedAt.After(found.CreatedAt) {
			found = &b
		}
	}
	if found == nil {
		return &node.Reply{Body: out}, nil
	}
	answer := found.Order.Confirmation
	out.OrderRef, out.OrderMessageID, out.Answer = found.Ref, found.Order.MessageID, &answer
	if answer.Status == intents.StatusConfirmed {
		var recs []store.Record
		if s.Log != nil {
			recs = s.Log.List(found.Operator, found.Ref)
		}
		out.EstimatedDelivery = store.Summarize(*found, recs).ETA
	}
	return &node.Reply{Body: out, BookingRef: found.Ref}, nil
}
