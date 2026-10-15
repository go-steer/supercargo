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

// Package intents is what each ULP intent means: its body type, the rules
// its envelope header follows, and the checks its body must pass
// (docs/DESIGN.md, section A.5).
//
// Decode is the one entry point. It takes a verified envelope and returns a
// typed, validated body, or an error wrapping ErrUnsupportedIntent or
// ErrInvalidBody. Bodies are decoded with exact member names, so a signed
// {"shipperRef": "A", "ShipperRef": "B"} is rejected rather than read as
// "B" here and "A" by another implementation.
package intents

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/internal/logsafe"
	"github.com/go-steer/supercargo/pkg/internal/strictjson"
)

// Intent names (docs/DESIGN.md, section A.5).
const (
	BookingRequest      = "ulp.ocean.booking.request"
	BookingConfirmation = "ulp.ocean.booking.confirmation"
	TnTEvent            = "ulp.tnt.event"
	TnTAlert            = "ulp.tnt.alert"
	TnTInquiry          = "ulp.tnt.inquiry"
	TnTInquiryResponse  = "ulp.tnt.inquiry.response"
	OceanTnTRequest     = "ulp.ocean.tnt.request"
	OceanTnTResponse    = "ulp.ocean.tnt.response"
	DelegationGrant     = "ulp.delegation.grant"
	Error               = "ulp.error"
)

var (
	// ErrUnsupportedIntent reports an intent this package has no body type
	// for. It maps to the ulp.error code "unsupported_intent".
	ErrUnsupportedIntent = errors.New("intents: unsupported intent")

	// ErrInvalidBody reports a body, or a header field the intent governs,
	// that breaks its rules. It maps to the ulp.error code "invalid_body".
	ErrInvalidBody = errors.New("intents: invalid body")
)

// Body is a decoded, validated intent body.
type Body interface {
	// Intent is the intent the body belongs to.
	Intent() string
	// Validate checks the body's own rules.
	Validate() error
}

// presence says whether an optional header field must, may or must not be
// set for an intent.
type presence int

const (
	forbidden presence = iota
	optional
	required
)

func (p presence) check(set bool, field string) error {
	switch {
	case p == required && !set:
		return fmt.Errorf("header.%s is required", field)
	case p == forbidden && set:
		return fmt.Errorf("header.%s is not allowed", field)
	}
	return nil
}

// spec is an intent's header rules and body type.
type spec struct {
	bookingRef presence
	inReplyTo  presence
	delegation presence
	// dialect is the payload schema the header must name, for an intent
	// with a standard payload; nil forbids a dialect (a ULP extension).
	dialect *envelope.Dialect
	newBody func() Body
}

// specs are the intents this package decodes. Delegation bodies join with
// the milestone that uses them.
var specs = map[string]spec{
	BookingRequest:      {bookingRef: forbidden, inReplyTo: forbidden, delegation: forbidden, newBody: func() Body { return &BookingRequestBody{} }},
	BookingConfirmation: {bookingRef: required, inReplyTo: required, delegation: forbidden, dialect: &DialectDCSAEBL, newBody: func() Body { return &BookingConfirmationBody{} }},
	Error:               {bookingRef: optional, inReplyTo: required, delegation: forbidden, newBody: func() Body { return &ErrorBody{} }},
	TnTEvent:            {bookingRef: required, inReplyTo: forbidden, delegation: forbidden, dialect: &DialectDCSATnT, newBody: func() Body { return &TnTEventBody{} }},
	TnTAlert:            {bookingRef: required, inReplyTo: forbidden, delegation: forbidden, newBody: func() Body { return &TnTAlertBody{} }},
	TnTInquiry:          {bookingRef: required, inReplyTo: forbidden, delegation: optional, newBody: func() Body { return &TnTInquiryBody{} }},
	TnTInquiryResponse:  {bookingRef: required, inReplyTo: required, delegation: forbidden, newBody: func() Body { return &TnTInquiryResponseBody{} }},
	OceanTnTRequest:     {bookingRef: required, inReplyTo: forbidden, delegation: optional, dialect: &DialectDCSATnT, newBody: func() Body { return &TnTStatusRequestBody{} }},
	OceanTnTResponse:    {bookingRef: required, inReplyTo: required, delegation: forbidden, dialect: &DialectDCSATnT, newBody: func() Body { return &TnTStatusResponseBody{} }},
	// A truck leg: requested (no booking yet), confirmed in reply to a
	// request or an amendment, and amended by the shipper of the leg.
	RoadBookingRequest:      {bookingRef: forbidden, inReplyTo: forbidden, delegation: forbidden, dialect: &DialectULPRoad, newBody: func() Body { return &RoadBookingRequestBody{} }},
	RoadBookingConfirmation: {bookingRef: required, inReplyTo: required, delegation: forbidden, dialect: &DialectULPRoad, newBody: func() Body { return &RoadBookingConfirmationBody{} }},
	RoadBookingAmendment:    {bookingRef: required, inReplyTo: forbidden, delegation: forbidden, dialect: &DialectULPRoad, newBody: func() Body { return &RoadBookingAmendmentBody{} }},
	// A booking (an ocean booking or a truck leg) withdrawn by its shipper,
	// and the carrier's answer.
	BookingCancellation:         {bookingRef: required, inReplyTo: forbidden, delegation: forbidden, newBody: func() Body { return &BookingCancellationBody{} }},
	BookingCancellationResponse: {bookingRef: required, inReplyTo: required, delegation: forbidden, newBody: func() Body { return &BookingCancellationResponseBody{} }},
	// A door-to-door order to a forwarder: requested (no order yet), and
	// confirmed with the forwarder's reference and the planned journey.
	ForwardingRequest:      {bookingRef: forbidden, inReplyTo: forbidden, delegation: forbidden, newBody: func() Body { return &ForwardingRequestBody{} }},
	ForwardingConfirmation: {bookingRef: required, inReplyTo: required, delegation: forbidden, newBody: func() Body { return &ForwardingConfirmationBody{} }},
	// Where an order stands, asked by the shipper's own reference (it may
	// not have the forwarder's: its first answer may have been lost), and
	// answered with the order's reference in the header when found.
	ForwardingStatusRequest:  {bookingRef: forbidden, inReplyTo: forbidden, delegation: forbidden, newBody: func() Body { return &ForwardingStatusRequestBody{} }},
	ForwardingStatusResponse: {bookingRef: optional, inReplyTo: required, delegation: forbidden, newBody: func() Body { return &ForwardingStatusResponseBody{} }},
}

// Supported reports whether Decode has a body type for intent.
func Supported(intent string) bool {
	_, ok := specs[intent]
	return ok
}

// Decode checks e's header against its intent's rules, decodes the body
// with exact member names, and validates it. e must already be verified:
// this checks meaning, not authenticity.
func Decode(e *envelope.Envelope) (Body, error) {
	s, ok := specs[e.Header.Intent]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedIntent, logsafe.Quote(e.Header.Intent))
	}
	if err := errors.Join(
		s.bookingRef.check(e.Header.BookingRef != nil, "bookingRef"),
		s.inReplyTo.check(e.Header.InReplyTo != nil, "inReplyTo"),
		s.delegation.check(len(e.Attachments.Delegation) > 0, "attachments.delegation"),
		checkDialect(s.dialect, e.Header.Dialect),
	); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrInvalidBody, e.Header.Intent, err)
	}
	b := s.newBody()
	if err := strictjson.Unmarshal(e.Body, b); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrInvalidBody, e.Header.Intent, err)
	}
	if err := b.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrInvalidBody, e.Header.Intent, err)
	}
	if err := crossCheck(&e.Header, b); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrInvalidBody, e.Header.Intent, err)
	}
	return b, nil
}

// DecodeBody decodes raw as intent's body with exact member names, and
// validates it: Decode's checks on the body alone, for a body that doesn't
// arrive in an envelope (a company's own system handing its node an event
// to sign and publish). The header's rules don't apply; the caller builds
// the header.
func DecodeBody(intent string, raw []byte) (Body, error) {
	s, ok := specs[intent]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedIntent, logsafe.Quote(intent))
	}
	b := s.newBody()
	if err := strictjson.Unmarshal(raw, b); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrInvalidBody, intent, err)
	}
	if err := b.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrInvalidBody, intent, err)
	}
	return b, nil
}

// checkDialect requires the header to name exactly want (standard and
// version; the schema URL is informative), or no dialect when want is nil.
func checkDialect(want, got *envelope.Dialect) error {
	switch {
	case want == nil && got != nil:
		return errors.New("header.dialect is not allowed")
	case want == nil:
		return nil
	case got == nil:
		return fmt.Errorf("header.dialect is required (%s %s)", want.Standard, want.Version)
	case got.Standard != want.Standard || got.Version != want.Version:
		return fmt.Errorf("header.dialect %s %s is not %s %s", logsafe.Quote(got.Standard), logsafe.Quote(got.Version), want.Standard, want.Version)
	}
	return nil
}

// HeaderFor returns a new header for intent from sender to receiver,
// naming the intent's payload dialect if it has one.
func HeaderFor(intent, sender, receiver string, now time.Time) envelope.Header {
	h := envelope.NewHeader(intent, sender, receiver, now)
	SetDialect(&h)
	return h
}

// SetDialect sets h's dialect to the one h's intent requires (or none).
func SetDialect(h *envelope.Header) {
	h.Dialect = nil
	if s, ok := specs[h.Intent]; ok && s.dialect != nil {
		d := *s.dialect
		h.Dialect = &d
	}
}

// DialectFor is the dialect intent's header carries, if it has one.
func DialectFor(intent string) (envelope.Dialect, bool) {
	if s, ok := specs[intent]; ok && s.dialect != nil {
		return *s.dialect, true
	}
	return envelope.Dialect{}, false
}

// Dialects are the standards of the dialects this package decodes, sorted:
// what a profile's supported_dialects lists.
func Dialects() []string {
	set := map[string]bool{}
	for _, s := range specs {
		if s.dialect != nil {
			set[s.dialect.Standard] = true
		}
	}
	return slices.Sorted(maps.Keys(set))
}

// crossCheck applies rules that tie a body to its header.
func crossCheck(h *envelope.Header, b Body) error {
	if c, ok := b.(*BookingConfirmationBody); ok && c.BookingRef != *h.BookingRef {
		return fmt.Errorf("body.carrier_booking_reference %s is not header.bookingRef %s", logsafe.Quote(c.BookingRef), logsafe.Quote(*h.BookingRef))
	}
	if c, ok := b.(*RoadBookingConfirmationBody); ok && c.BookingRef != *h.BookingRef {
		return fmt.Errorf("body.carrier_booking_reference %s is not header.bookingRef %s", logsafe.Quote(c.BookingRef), logsafe.Quote(*h.BookingRef))
	}
	if r, ok := b.(*RoadBookingRequestBody); ok && r.Shipper.Domain != "" && r.Shipper.Domain != h.Sender {
		// The shipper of a leg is whoever signs its request.
		return fmt.Errorf("body.shipper.domain %s is not header.sender %s", logsafe.Quote(r.Shipper.Domain), logsafe.Quote(h.Sender))
	}
	if r, ok := b.(*ForwardingRequestBody); ok && r.Shipper.Domain != "" && r.Shipper.Domain != h.Sender {
		// The shipper of an order is whoever signs it.
		return fmt.Errorf("body.shipper.domain %s is not header.sender %s", logsafe.Quote(r.Shipper.Domain), logsafe.Quote(h.Sender))
	}
	if c, ok := b.(*ForwardingConfirmationBody); ok && c.OrderRef != *h.BookingRef {
		return fmt.Errorf("body.forwarder_reference %s is not header.bookingRef %s", logsafe.Quote(c.OrderRef), logsafe.Quote(*h.BookingRef))
	}
	if c, ok := b.(*BookingCancellationBody); ok && c.BookingRef != *h.BookingRef {
		return fmt.Errorf("body.carrier_booking_reference %s is not header.bookingRef %s", logsafe.Quote(c.BookingRef), logsafe.Quote(*h.BookingRef))
	}
	if c, ok := b.(*BookingCancellationResponseBody); ok && c.BookingRef != *h.BookingRef {
		return fmt.Errorf("body.carrier_booking_reference %s is not header.bookingRef %s", logsafe.Quote(c.BookingRef), logsafe.Quote(*h.BookingRef))
	}
	if r, ok := b.(*ForwardingStatusResponseBody); ok {
		// The header names the order found, and nothing when none was.
		switch {
		case r.Found() && (h.BookingRef == nil || *h.BookingRef != r.OrderRef):
			return fmt.Errorf("header.bookingRef is not body.forwarder_reference %s", logsafe.Quote(r.OrderRef))
		case !r.Found() && h.BookingRef != nil:
			return fmt.Errorf("header.bookingRef %s names an order not found", logsafe.Quote(*h.BookingRef))
		}
	}
	if a, ok := b.(*RoadBookingAmendmentBody); ok && a.BookingRef != *h.BookingRef {
		return fmt.Errorf("body.carrier_booking_reference %s is not header.bookingRef %s", logsafe.Quote(a.BookingRef), logsafe.Quote(*h.BookingRef))
	}
	if r, ok := b.(*TnTInquiryResponseBody); ok && r.DataScope.BookingRef != *h.BookingRef {
		return fmt.Errorf("body.dataScope.bookingRef %s is not header.bookingRef %s", logsafe.Quote(r.DataScope.BookingRef), logsafe.Quote(*h.BookingRef))
	}
	return nil
}
