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

// Package forwarder is a freight forwarder's side of a door-to-door order
// (docs/proposals/door-to-door-forwarder.md): it takes orders from
// shippers as their operator, plans and books each leg as the legs'
// shipper, and answers the order with the journey.
package forwarder

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
)

// Orders takes ulp.forwarding.request as the order's operator: each order
// is recorded, given its reference, and answered "pending" at once; its
// plan follows when a person at the forwarder confirms it (Answer).
type Orders struct {
	Node     *node.Node
	Bookings *store.Bookings
	// RefPrefix starts each order reference, e.g. "MF".
	RefPrefix string
	// Deny lists domains whose orders are refused (A.6: as booking
	// requests, anyone may order; the forwarder may decline).
	Deny map[string]bool
	Now  func() time.Time
	// OnOrder learns of each order taken; OnAnswer of each answered.
	// Optional; they must not block.
	OnOrder  func(store.Booking)
	OnAnswer func(store.Booking)

	mu        sync.Mutex
	next      int
	answering map[string]bool // orders whose answer is being sent
}

// Authorize implements node.Handler: the sender is not denied, and a
// subscription points at its own domain (A.6).
func (o *Orders) Authorize(_ context.Context, req *node.Request) error {
	sender := req.Envelope.Header.Sender
	if o.Deny[sender] {
		return node.Reject(intents.CodeForbidden, "This forwarder does not take orders from the sender.", "sender is on the deny list")
	}
	b := req.Body.(*intents.ForwardingRequestBody)
	if b.Subscription != nil && b.Subscription.EndpointHost() != sender {
		return node.Reject(intents.CodeForbidden, "The subscription endpoint must be on the sender's own domain.",
			"subscription endpoint host "+b.Subscription.EndpointHost()+" is not "+sender)
	}
	return nil
}

// Handle implements node.Handler: the order is recorded under a new
// reference and answered pending, with its subscription.
func (o *Orders) Handle(_ context.Context, req *node.Request) (*node.Reply, error) {
	h := req.Envelope.Header
	b := req.Body.(*intents.ForwardingRequestBody)
	o.mu.Lock()
	if o.next == 0 {
		o.next = 7
	}
	n := o.next
	o.next++
	o.mu.Unlock()
	ref := fmt.Sprintf("%s-%04d", o.RefPrefix, n)
	conf := &intents.ForwardingConfirmationBody{OrderRef: ref, Status: intents.StatusPending}
	if b.Subscription != nil {
		conf.SubscriptionID = "SUB-" + ref
	}
	order := store.Booking{Ref: ref, Shipper: h.Sender, Operator: h.Receiver, CreatedAt: o.Now(),
		Order: &store.OrderBooking{Request: *b, MessageID: h.MessageID, Confirmation: *conf}}
	if err := o.Bookings.Add(order); errors.Is(err, store.ErrFull) {
		return &node.Reply{BookingRef: ref, Body: &intents.ForwardingConfirmationBody{OrderRef: ref, Status: intents.StatusRejected,
			Messages: []intents.Message{{Code: "at_capacity", Detail: "This forwarder is not taking new orders right now."}}}}, nil
	} else if err != nil {
		return nil, fmt.Errorf("forwarder: recording order %s: %w", ref, err)
	}
	if o.OnOrder != nil {
		if stored, err := o.Bookings.Get(h.Receiver, ref); err == nil {
			o.OnOrder(stored)
		}
	}
	return &node.Reply{BookingRef: ref, Body: conf}, nil
}

var (
	// ErrUnknownOrder is an answer to an order this forwarder doesn't hold.
	ErrUnknownOrder = errors.New("forwarder: no such order")
	// ErrAnswered is an answer to an order already confirmed or rejected.
	ErrAnswered = errors.New("forwarder: the order is answered already")
	// ErrInvalidAnswer is an answer that isn't a valid final answer.
	ErrInvalidAnswer = errors.New("forwarder: not a valid answer")
)

// Answer sends a pending order's final answer to its shipper, in reply to
// the order: the planned journey (confirmed) or a rejection. It is the
// deterministic send step a person at the forwarder confirms; no model
// calls it. The order records the answer only once the shipper's node
// accepted it. If that acceptance was lost on the way, sending the same
// answer again is safe: the shipper takes an answer it already holds
// without changing anything.
func (o *Orders) Answer(ctx context.Context, ref string, conf *intents.ForwardingConfirmationBody) error {
	if conf.Status == intents.StatusPending {
		return fmt.Errorf("%w: the final answer is confirmed or rejected", ErrInvalidAnswer)
	}
	// One answer at a time per order: two sent at once would both reach
	// the shipper, which takes only the first.
	o.mu.Lock()
	if o.answering[ref] {
		o.mu.Unlock()
		return fmt.Errorf("%w: %s is being answered", ErrAnswered, ref)
	}
	if o.answering == nil {
		o.answering = map[string]bool{}
	}
	o.answering[ref] = true
	o.mu.Unlock()
	defer func() {
		o.mu.Lock()
		delete(o.answering, ref)
		o.mu.Unlock()
	}()
	b, err := o.Bookings.Get(o.Node.Domain(), ref)
	if err != nil || b.Order == nil {
		return fmt.Errorf("%w: %s", ErrUnknownOrder, ref)
	}
	if b.Order.Confirmation.Status != intents.StatusPending {
		return fmt.Errorf("%w: %s is %s", ErrAnswered, ref, b.Order.Confirmation.Status)
	}
	conf.OrderRef = ref
	if conf.Status == intents.StatusConfirmed {
		// The subscription the order was given when taken stays its own.
		conf.SubscriptionID = b.Order.Confirmation.SubscriptionID
	}
	if err := conf.Validate(); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrInvalidAnswer, ref, err)
	}
	if conf.Status == intents.StatusConfirmed {
		// Held to the order as the shipper will hold it, so a plan it
		// would refuse is caught here, before anything is sent.
		if err := intents.FitsOrder(&b.Order.Request, conf); err != nil {
			return fmt.Errorf("%w: %s: %w", ErrInvalidAnswer, ref, err)
		}
	}
	h := envelope.NewHeader(intents.ForwardingConfirmation, o.Node.Domain(), b.Shipper, o.Now())
	h.BookingRef = &ref
	h.InReplyTo = &b.Order.MessageID
	if _, err := o.Node.Exchange(ctx, h, conf); err != nil {
		return err
	}
	snapshot := b.Order
	stored, err := o.Bookings.Update(o.Node.Domain(), ref, func(b *store.Booking) error {
		if !reflect.DeepEqual(b.Order, snapshot) {
			return ErrAnswered // a second answer raced this one
		}
		b.Order.Confirmation = *conf
		return nil
	})
	if err != nil {
		return err
	}
	if o.OnAnswer != nil {
		o.OnAnswer(stored)
	}
	return nil
}
