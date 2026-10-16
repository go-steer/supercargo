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
	"fmt"
	"reflect"

	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
)

// Order sends a door-to-door order to a freight forwarder and returns its
// answer. An order the forwarder takes (pending, with its reference, or
// confirmed with its plan) is recorded under that reference, with the ID
// of the message it went in: a pending order's final answer must reply to
// it. A rejection is returned for the person to see.
//
// Like Book, it is a deterministic send step, run only after a person
// confirmed the order; no model calls it.
func (s *Shipper) Order(ctx context.Context, forwarder string, req *intents.ForwardingRequestBody) (*intents.ForwardingConfirmationBody, error) {
	if s.NoBooking {
		return nil, ErrNoShipperRole
	}
	h := envelope.NewHeader(intents.ForwardingRequest, s.Node.Domain(), forwarder, s.Now())
	// Kept until its first answer is recorded: if that answer is lost, a
	// status request can still find the order (OrderStatus).
	if err := s.rememberSent(h.MessageID, forwarder, req); err != nil {
		return nil, err
	}
	resp, err := s.Node.Exchange(ctx, h, req, intents.ForwardingConfirmation)
	if err != nil {
		return nil, err
	}
	conf, ok := resp.Body.(*intents.ForwardingConfirmationBody)
	if !ok {
		return nil, errors.New("shipper: response is not a forwarding confirmation")
	}
	if conf.Status == intents.StatusRejected {
		s.forgetSent(h.MessageID)
		return conf, nil
	}
	if err := checkFirstAnswer(forwarder, req, conf); err != nil {
		return nil, err
	}
	if _, err := s.record(forwarder, req, h.MessageID, conf); err != nil {
		return nil, err
	}
	return conf, nil
}

// record holds the order sent to forwarder in message id, with its first
// answer conf, and stops awaiting it. An order already held from that
// message (taken up meanwhile by a status request, or recorded by its
// first answer) is reported held; any other under the reference is an
// error.
func (s *Shipper) record(forwarder string, req *intents.ForwardingRequestBody, id string, conf *intents.ForwardingConfirmationBody) (held bool, err error) {
	err = s.Bookings.Add(store.Booking{
		Ref: conf.OrderRef, Shipper: s.Node.Domain(), Operator: forwarder,
		Order:     &store.OrderBooking{Request: *req, MessageID: id, Confirmation: *conf},
		CreatedAt: s.Now(),
	})
	if errors.Is(err, store.ErrExists) {
		if !s.heldOrder(forwarder, conf.OrderRef, id) {
			return false, fmt.Errorf("shipper: %s took %s again: %w", forwarder, conf.OrderRef, err)
		}
		held, err = true, nil
	}
	if err != nil {
		return false, fmt.Errorf("shipper: recording %s's answer to %s: %w", forwarder, req.ShipperRef, err)
	}
	s.forgetSent(id)
	return held, nil
}

// OrderAnswers handles ulp.forwarding.confirmation pushed by a forwarder:
// the final answer (confirmed or rejected) to an order it took as pending.
type OrderAnswers struct {
	Bookings *store.Bookings
	// OnAnswer learns of each order answered. Optional; it must not block.
	OnAnswer func(store.Booking)
}

// Authorize implements node.Handler (docs/DESIGN.md, A.6): the order is
// one we hold with the sender as its forwarder (looked up under the
// sender, so another party never finds it), still pending, and the answer
// replies to the message the order went in. Every failure is the same
// forbidden.
func (o *OrderAnswers) Authorize(_ context.Context, req *node.Request) error {
	h := req.Envelope.Header
	b, err := o.Bookings.Get(h.Sender, *h.BookingRef)
	reason := ""
	switch {
	case err != nil:
		reason = "no order " + *h.BookingRef + " with forwarder " + h.Sender
	case b.Order == nil:
		reason = *h.BookingRef + " is not an order"
	case b.Order.Confirmation.Status != intents.StatusPending && *h.InReplyTo == b.Order.MessageID &&
		reflect.DeepEqual(b.Order.Confirmation, *req.Body.(*intents.ForwardingConfirmationBody)):
		// The same answer again: a forwarder whose first push lost its
		// reply resends it. Taken, changing nothing, so both sides agree.
		return nil
	case b.Order.Confirmation.Status != intents.StatusPending:
		reason = "order " + b.Ref + " is already " + b.Order.Confirmation.Status
	case *h.InReplyTo != b.Order.MessageID:
		reason = "the answer doesn't reply to order " + b.Ref
	case req.Body.(*intents.ForwardingConfirmationBody).Status == intents.StatusPending:
		reason = "order " + b.Ref + " is pending already"
	default:
		return nil
	}
	return &node.Rejection{Code: forbidden.Code, Detail: forbidden.Detail, Reason: reason}
}

// Handle implements node.Handler: a confirmed plan is held to the order
// as Order holds a first answer, then recorded; a rejection is recorded.
// A plan that doesn't fit is refused, and the order stays pending.
func (o *OrderAnswers) Handle(_ context.Context, req *node.Request) (*node.Reply, error) {
	h := req.Envelope.Header
	conf := req.Body.(*intents.ForwardingConfirmationBody)
	b, err := o.Bookings.Get(h.Sender, *h.BookingRef)
	if err != nil {
		return nil, err // authorized a moment ago
	}
	if b.Order.Confirmation.Status != intents.StatusPending {
		return nil, nil // the answer held already, resent (Authorize)
	}
	if b.Order.Request.Subscription == nil && conf.SubscriptionID != "" {
		return nil, node.Reject(intents.CodeInvalidBody, "The plan adds a subscription the order didn't ask for.", "subscription "+conf.SubscriptionID)
	}
	if conf.Status == intents.StatusConfirmed {
		if err := intents.FitsOrder(&b.Order.Request, conf); err != nil {
			return nil, node.Reject(intents.CodeInvalidBody, "The plan doesn't fit the order.", err.Error())
		}
	}
	if sub := b.Order.Confirmation.SubscriptionID; sub != "" && conf.Status == intents.StatusConfirmed && conf.SubscriptionID != sub {
		return nil, node.Reject(intents.CodeInvalidBody, "The plan changes the order's subscription.", "subscription "+conf.SubscriptionID+" is not "+sub)
	}
	snapshot := b.Order
	stored, err := o.Bookings.Update(h.Sender, *h.BookingRef, func(b *store.Booking) error {
		if !reflect.DeepEqual(b.Order, snapshot) {
			return errors.New("shipper: the order changed meanwhile") // a second answer raced this one
		}
		b.Order.Confirmation = *conf
		return nil
	})
	if err != nil {
		return nil, err
	}
	if o.OnAnswer != nil {
		o.OnAnswer(stored)
	}
	return nil, nil
}
