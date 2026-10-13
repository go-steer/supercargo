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
	resp, err := s.Node.Exchange(ctx, h, req, intents.ForwardingConfirmation)
	if err != nil {
		return nil, err
	}
	conf, ok := resp.Body.(*intents.ForwardingConfirmationBody)
	if !ok {
		return nil, errors.New("shipper: response is not a forwarding confirmation")
	}
	if conf.Status == intents.StatusRejected {
		return conf, nil
	}
	if req.Subscription != nil && conf.SubscriptionID == "" {
		return nil, fmt.Errorf("%w: %s's answer to %s has no subscription_id", ErrPlanMismatch, forwarder, req.ShipperRef)
	}
	if conf.Status == intents.StatusConfirmed {
		if err := intents.FitsOrder(req, conf); err != nil {
			return nil, fmt.Errorf("%w: %s's answer to %s: %w", ErrPlanMismatch, forwarder, req.ShipperRef, err)
		}
	}
	if err := s.Bookings.Add(store.Booking{
		Ref: conf.OrderRef, Shipper: s.Node.Domain(), Operator: forwarder,
		Order:     &store.OrderBooking{Request: *req, MessageID: h.MessageID, Confirmation: *conf},
		CreatedAt: s.Now(),
	}); errors.Is(err, store.ErrExists) {
		return nil, fmt.Errorf("shipper: %s took %s again: %w", forwarder, conf.OrderRef, err)
	} else if err != nil {
		return nil, fmt.Errorf("shipper: recording %s's answer to %s: %w", forwarder, req.ShipperRef, err)
	}
	return conf, nil
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
