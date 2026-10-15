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

package intents

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/go-steer/supercargo/pkg/internal/logsafe"
)

// The forwarding status intents (ULP's own, like the order's): the
// shipper asks its forwarder where an order stands, by its own reference,
// which it has even if the forwarder's first answer never reached it.
const (
	ForwardingStatusRequest  = "ulp.forwarding.status.request"
	ForwardingStatusResponse = "ulp.forwarding.status.response"
)

// messageID is an envelope's messageId: a lowercase UUIDv4.
var messageID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// ForwardingStatusRequestBody is the body of ulp.forwarding.status.request:
// the shipper's own reference for the order.
type ForwardingStatusRequestBody struct {
	ShipperRef string `json:"shipper_reference"`
}

// ForwardingStatusResponseBody is the body of
// ulp.forwarding.status.response: the order the sender placed under that
// reference, if any (the latest, if several): the forwarder's reference,
// the ID of the signed order message it holds (the shipper matches it to
// what it sent), its current answer (pending, confirmed with the journey,
// or rejected), and the delivery as estimated now. None of it, but the
// reference asked about, if there is no such order.
type ForwardingStatusResponseBody struct {
	ShipperRef        string                      `json:"shipper_reference"`
	OrderRef          string                      `json:"forwarder_reference,omitempty"`
	OrderMessageID    string                      `json:"order_message_id,omitempty"`
	Answer            *ForwardingConfirmationBody `json:"answer,omitempty"`
	EstimatedDelivery string                      `json:"estimated_delivery,omitempty"`
}

// Intent implements Body.
func (*ForwardingStatusRequestBody) Intent() string { return ForwardingStatusRequest }

// Intent implements Body.
func (*ForwardingStatusResponseBody) Intent() string { return ForwardingStatusResponse }

// Validate implements Body.
func (b *ForwardingStatusRequestBody) Validate() error { return ref("shipper_reference", b.ShipperRef) }

// Found reports whether the response names an order.
func (b *ForwardingStatusResponseBody) Found() bool { return b.Answer != nil }

// Validate implements Body: a found order has its reference, its message's
// ID and an answer about it; a not-found one nothing but the reference.
func (b *ForwardingStatusResponseBody) Validate() error {
	errs := []error{ref("shipper_reference", b.ShipperRef)}
	if b.Answer == nil {
		if b.OrderRef != "" || b.OrderMessageID != "" || b.EstimatedDelivery != "" {
			errs = append(errs, errors.New("a response with no order carries nothing but the reference asked about"))
		}
		return errors.Join(errs...)
	}
	errs = append(errs, ref("forwarder_reference", b.OrderRef), b.Answer.Validate())
	if b.Answer.OrderRef != b.OrderRef {
		errs = append(errs, fmt.Errorf("answer is about %s, not %s", logsafe.Quote(b.Answer.OrderRef), logsafe.Quote(b.OrderRef)))
	}
	if !messageID.MatchString(b.OrderMessageID) {
		errs = append(errs, fmt.Errorf("order_message_id: %s is not a message ID (a lowercase UUIDv4)", logsafe.Quote(b.OrderMessageID)))
	}
	if b.EstimatedDelivery != "" {
		_, err := timestamp("estimated_delivery", b.EstimatedDelivery)
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
