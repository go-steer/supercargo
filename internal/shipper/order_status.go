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
	"slices"
	"time"

	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
)

// An order sent, whose first answer hasn't been recorded (lost on the way,
// say), is kept here by the ID of the message it went in: a status answer
// naming that ID lets the shipper take the order up (OrderStatus).

// MaxSentOrders bounds the orders kept awaiting a first answer; the oldest
// are forgotten first.
const MaxSentOrders = 1000

// sentOrder is an order sent and not yet recorded.
type sentOrder struct {
	forwarder string
	request   intents.ForwardingRequestBody
	at        time.Time
}

// rememberSent keeps an order about to be sent, under its message ID.
func (s *Shipper) rememberSent(id, forwarder string, req *intents.ForwardingRequestBody) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sent == nil {
		s.sent = map[string]sentOrder{}
	}
	s.sent[id] = sentOrder{forwarder: forwarder, request: *req, at: s.Now()}
	s.sentOrder = append(s.sentOrder, id)
	for len(s.sentOrder) > MaxSentOrders {
		delete(s.sent, s.sentOrder[0])
		s.sentOrder = s.sentOrder[1:]
	}
}

// forgetSent drops an order once its first answer is recorded.
func (s *Shipper) forgetSent(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sent, id)
	if i := slices.Index(s.sentOrder, id); i >= 0 {
		s.sentOrder = slices.Delete(s.sentOrder, i, i+1)
	}
}

// sentTo is the order sent in message id to forwarder, if it's kept.
func (s *Shipper) sentTo(id, forwarder string) (sentOrder, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.sent[id]
	return o, ok && o.forwarder == forwarder
}

// OrderStatus is where an order stands, as its forwarder says, and what
// the shipper made of it.
type OrderStatus struct {
	Forwarder string                                `json:"forwarder"`
	Status    *intents.ForwardingStatusResponseBody `json:"status"`
	// Held: the shipper holds the order already. Adopted: it didn't, and
	// took it up now: the order it sent, in the message the answer names.
	Held    bool `json:"held,omitempty"`
	Adopted bool `json:"adopted,omitempty"`
	// Unknown: the forwarder has an order under the reference that this
	// shipper has no record of sending (or no longer keeps): not taken up.
	Unknown bool `json:"unknown,omitempty"`
}

// ErrStatusMismatch is a status answer about something not asked.
var ErrStatusMismatch = errors.New("shipper: the status answer isn't about the order asked about")

// OrderStatus asks forwarder where the order under the shipper's own
// reference ref stands. If the shipper holds no such order but sent it,
// in the message the answer names (its first answer lost on the way), it
// takes the order up, held to the order as a first answer is (Order), so
// the forwarder's later answers are taken. It changes nothing otherwise.
func (s *Shipper) OrderStatus(ctx context.Context, forwarder, ref string) (*OrderStatus, error) {
	body := &intents.ForwardingStatusRequestBody{ShipperRef: ref}
	if err := body.Validate(); err != nil {
		return nil, fmt.Errorf("shipper: %w", err)
	}
	h := envelope.NewHeader(intents.ForwardingStatusRequest, s.Node.Domain(), forwarder, s.Now())
	resp, err := s.Node.Exchange(ctx, h, body, intents.ForwardingStatusResponse)
	if err != nil {
		return nil, err
	}
	st, ok := resp.Body.(*intents.ForwardingStatusResponseBody)
	if !ok {
		return nil, errors.New("shipper: response is not a forwarding status")
	}
	if st.ShipperRef != ref {
		return nil, fmt.Errorf("%w: asked about %s, answered about %s", ErrStatusMismatch, ref, st.ShipperRef)
	}
	out := &OrderStatus{Forwarder: forwarder, Status: st}
	if !st.Found() {
		return out, nil
	}
	// Decoding bound the header's bookingRef to st.OrderRef.
	if b, err := s.Bookings.Get(forwarder, st.OrderRef); err == nil {
		if b.Order == nil || b.Order.Request.ShipperRef != ref || b.Order.MessageID != st.OrderMessageID {
			return nil, fmt.Errorf("%w: %s is not our order %s", ErrStatusMismatch, st.OrderRef, ref)
		}
		out.Held = true
		return out, nil
	}
	sent, ok := s.sentTo(st.OrderMessageID, forwarder)
	if !ok || sent.request.ShipperRef != ref {
		out.Unknown = true
		return out, nil
	}
	conf := st.Answer
	if conf.Status == intents.StatusRejected {
		// Nothing to take up: the forwarder declined the order.
		s.forgetSent(st.OrderMessageID)
		return out, nil
	}
	if err := checkFirstAnswer(forwarder, &sent.request, conf); err != nil {
		return nil, err
	}
	held, err := s.record(forwarder, &sent.request, st.OrderMessageID, conf)
	if err != nil {
		return nil, err
	}
	out.Held, out.Adopted = held, !held
	return out, nil
}

// checkFirstAnswer holds a first answer to the order it answers: its
// subscription as asked, and a confirmed plan that fits.
func checkFirstAnswer(forwarder string, req *intents.ForwardingRequestBody, conf *intents.ForwardingConfirmationBody) error {
	if req.Subscription != nil && conf.SubscriptionID == "" {
		return fmt.Errorf("%w: %s's answer to %s has no subscription_id", ErrPlanMismatch, forwarder, req.ShipperRef)
	}
	if req.Subscription == nil && conf.SubscriptionID != "" {
		return fmt.Errorf("%w: %s's answer to %s has a subscription the order didn't ask for", ErrPlanMismatch, forwarder, req.ShipperRef)
	}
	if conf.Status == intents.StatusConfirmed {
		if err := intents.FitsOrder(req, conf); err != nil {
			return fmt.Errorf("%w: %s's answer to %s: %w", ErrPlanMismatch, forwarder, req.ShipperRef, err)
		}
	}
	return nil
}

// heldOrder reports whether the order held under forwarder's reference
// ref is the one sent in message id.
func (s *Shipper) heldOrder(forwarder, ref, id string) bool {
	b, err := s.Bookings.Get(forwarder, ref)
	return err == nil && b.Order != nil && b.Order.MessageID == id
}
