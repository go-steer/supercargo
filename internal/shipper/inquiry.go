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
	"time"

	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
)

// Answer is an operator's answer to an inquiry, with what this shipper can
// check of it.
type Answer struct {
	Operator string
	Response *intents.TnTInquiryResponseBody
	// Held are the cited records this shipper holds, as signed events or
	// alerts the operator pushed; the rest it has only the answer's word for.
	Held []string
}

// AskTimeout bounds an inquiry: longer than the 10s other exchanges get,
// since the operator's model answers it.
const AskTimeout = 45 * time.Second

// ErrNotOurBooking reports an inquiry about a booking this shipper doesn't
// hold with that operator: the node asks only about its own shipments.
var ErrNotOurBooking = errors.New("shipper: no such booking with that operator")

// Ask sends a ulp.tnt.inquiry about ref to operator and returns the
// verified answer. Unlike a booking it commits to nothing, so it needs no
// confirmation; it still asks only about bookings this shipper holds.
func (s *Shipper) Ask(ctx context.Context, operator, ref, question string) (*Answer, error) {
	if _, err := s.Bookings.Get(operator, ref); err != nil {
		return nil, fmt.Errorf("%w: %s with %s", ErrNotOurBooking, ref, operator)
	}
	body := &intents.TnTInquiryBody{Question: question}
	if err := body.Validate(); err != nil {
		return nil, fmt.Errorf("shipper: %w", err)
	}
	h := envelope.NewHeader(intents.TnTInquiry, s.Node.Domain(), operator, s.Now())
	h.BookingRef = &ref
	// The operator may spend two model attempts on the answer.
	ctx, cancel := context.WithTimeout(ctx, AskTimeout)
	defer cancel()
	resp, err := s.Node.ExchangeA2A(ctx, h, body, intents.TnTInquiryResponse)
	if err != nil {
		return nil, err
	}
	r, ok := resp.Body.(*intents.TnTInquiryResponseBody)
	if !ok {
		return nil, errors.New("shipper: response is not an inquiry response")
	}
	// The body's dataScope matches its header (intents.Decode); the header
	// must be the booking asked about.
	if got := resp.Envelope.Header.BookingRef; got == nil || *got != ref {
		return nil, errors.New("shipper: the answer is about another booking")
	}
	return &Answer{Operator: operator, Response: r, Held: s.held(operator, ref, r.CitedEventIDs)}, nil
}

// held is the subset of ids this shipper has recorded for the booking.
func (s *Shipper) held(operator, ref string, ids []string) []string {
	if s.Events == nil {
		return nil
	}
	have := map[string]bool{}
	for _, r := range s.Events.List(operator, ref) {
		have[r.ID] = true
	}
	var out []string
	for _, id := range ids {
		if have[id] {
			out = append(out, id)
		}
	}
	return out
}
