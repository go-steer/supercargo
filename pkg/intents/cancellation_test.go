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
	"encoding/json"
	"testing"

	"github.com/go-steer/supercargo/pkg/envelope"
)

// TestCancellation: a cancellation names a booking; its answer is
// cancelled, or refused with a reason.
func TestCancellation(t *testing.T) {
	if err := (&BookingCancellationBody{BookingRef: "SR-BKG-R-0003"}).Validate(); err != nil {
		t.Errorf("cancellation: %v", err)
	}
	if err := (&BookingCancellationBody{}).Validate(); err == nil {
		t.Error("a cancellation of nothing")
	}
	refused := []Message{{Code: "under_way", Detail: "The container was picked up."}}
	for name, b := range map[string]BookingCancellationResponseBody{
		"cancelled": {BookingRef: "SR-BKG-R-0003", Status: StatusCancelled},
		"refused":   {BookingRef: "SR-BKG-R-0003", Status: StatusRefused, Messages: refused},
	} {
		if err := b.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, b := range map[string]BookingCancellationResponseBody{
		"no reference":      {Status: StatusCancelled},
		"another status":    {BookingRef: "SR-BKG-R-0003", Status: StatusConfirmed},
		"refused, no why":   {BookingRef: "SR-BKG-R-0003", Status: StatusRefused},
		"too many messages": {BookingRef: "SR-BKG-R-0003", Status: StatusRefused, Messages: make([]Message, MaxMessages+1)},
	} {
		if err := b.Validate(); err == nil {
			t.Errorf("%s: valid", name)
		}
	}
	if (&BookingCancellationBody{}).Intent() != BookingCancellation || (&BookingCancellationResponseBody{}).Intent() != BookingCancellationResponse ||
		!Supported(BookingCancellation) || !Supported(BookingCancellationResponse) {
		t.Error("intents")
	}
}

// TestDecodeCancellation: both decode with the header naming the booking
// the body names, and nothing else.
func TestDecodeCancellation(t *testing.T) {
	c := envelope.NewHeader(BookingCancellation, "meridian-forwarding.example", "serra-transportes.example", now)
	c.BookingRef = ptr("SR-BKG-R-0003")
	body := json.RawMessage(`{"carrier_booking_reference":"SR-BKG-R-0003"}`)
	if b, err := Decode(&envelope.Envelope{Header: c, Body: body}); err != nil || b.(*BookingCancellationBody).BookingRef != "SR-BKG-R-0003" {
		t.Errorf("cancellation: %+v, %v", b, err)
	}
	r := envelope.NewHeader(BookingCancellationResponse, "serra-transportes.example", "meridian-forwarding.example", now)
	r.BookingRef, r.InReplyTo = ptr("SR-BKG-R-0003"), ptr("m-1")
	answer := json.RawMessage(`{"carrier_booking_reference":"SR-BKG-R-0003","status":"cancelled"}`)
	if b, err := Decode(&envelope.Envelope{Header: r, Body: answer}); err != nil || b.(*BookingCancellationResponseBody).Status != StatusCancelled {
		t.Errorf("response: %+v, %v", b, err)
	}
	other := ptr("SR-BKG-R-0004")
	for name, e := range map[string]*envelope.Envelope{
		"cancellation of another":   {Header: with(c, func(h *envelope.Header) { h.BookingRef = other }), Body: body},
		"cancellation of none":      {Header: with(c, func(h *envelope.Header) { h.BookingRef = nil }), Body: body},
		"cancellation in reply":     {Header: with(c, func(h *envelope.Header) { h.InReplyTo = ptr("m-1") }), Body: body},
		"answer about another":      {Header: with(r, func(h *envelope.Header) { h.BookingRef = other }), Body: answer},
		"answer not in reply":       {Header: with(r, func(h *envelope.Header) { h.InReplyTo = nil }), Body: answer},
		"answer with a dialect":     {Header: with(r, func(h *envelope.Header) { h.Dialect = &DialectULPRoad }), Body: answer},
		"cancellation, with reason": {Header: c, Body: json.RawMessage(`{"carrier_booking_reference":"SR-BKG-R-0003","reason":"x"}`)},
	} {
		if _, err := Decode(e); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
}

func with(h envelope.Header, edit func(*envelope.Header)) envelope.Header {
	edit(&h)
	return h
}
