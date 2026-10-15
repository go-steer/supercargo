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

	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
)

var (
	// ErrCancelled reports a booking already cancelled.
	ErrCancelled = errors.New("shipper: the booking was cancelled")
	// ErrLegOfOrder reports a leg a forwarding order holds: cancelling it
	// is the order's business, not a booking's.
	ErrLegOfOrder = errors.New("shipper: the booking is a leg of an order")
)

// Cancel sends a cancellation of one of our bookings (an ocean booking or
// a truck leg) to its carrier (docs/DESIGN.md, A.5 "Booking
// cancellation"). The booking is recorded as cancelled only if the
// carrier answers so; a refusal leaves it as it was, and is returned for
// the person to see. Like Book, it is a deterministic send step, run only
// after a person confirmed; no model calls it.
func (s *Shipper) Cancel(ctx context.Context, carrier, ref string) (*intents.BookingCancellationResponseBody, error) {
	b, err := s.Bookings.Get(carrier, ref)
	if err != nil || b.Order != nil {
		return nil, fmt.Errorf("%w: booking %s with %s", ErrNotOurBooking, ref, carrier)
	}
	if b.Cancelled() {
		return nil, fmt.Errorf("%w: %s with %s", ErrCancelled, ref, carrier)
	}
	if s.InOrder != nil && s.InOrder(carrier, ref) {
		return nil, fmt.Errorf("%w: %s with %s", ErrLegOfOrder, ref, carrier)
	}
	h := envelope.NewHeader(intents.BookingCancellation, s.Node.Domain(), carrier, s.Now())
	h.BookingRef = &ref
	resp, err := s.Node.Exchange(ctx, h, &intents.BookingCancellationBody{BookingRef: ref}, intents.BookingCancellationResponse)
	if err != nil {
		return nil, err
	}
	a, ok := resp.Body.(*intents.BookingCancellationResponseBody)
	if !ok {
		return nil, errors.New("shipper: response is not a cancellation response")
	}
	if a.BookingRef != ref {
		// The node checks the answer replies to our message, not which
		// booking it names: an answer about another isn't this one's.
		return nil, fmt.Errorf("%w: %s answered a cancellation of %s for %s", ErrPlanMismatch, carrier, ref, a.BookingRef)
	}
	if a.Status != intents.StatusCancelled {
		return a, nil
	}
	at := s.Now()
	if _, err := s.Bookings.Update(carrier, ref, func(b *store.Booking) error {
		if b.CancelledAt.IsZero() {
			b.CancelledAt = at
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("shipper: recording %s's cancellation of %s: %w", carrier, ref, err)
	}
	return a, nil
}
