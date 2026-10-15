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

	"github.com/go-steer/supercargo/pkg/internal/logsafe"
)

// The booking cancellation intents (ULP's own, any mode): the shipper of
// an ocean booking or a truck leg withdraws it, and the carrier answers.
const (
	BookingCancellation         = "ulp.booking.cancellation"
	BookingCancellationResponse = "ulp.booking.cancellation.response"
)

// Cancellation answers.
const (
	StatusCancelled = "cancelled"
	StatusRefused   = "refused"
)

// BookingCancellationBody is the body of ulp.booking.cancellation: the
// booking withdrawn, and never why (that's the shipper's business).
type BookingCancellationBody struct {
	BookingRef string `json:"carrier_booking_reference"`
}

// BookingCancellationResponseBody is the body of
// ulp.booking.cancellation.response: cancelled, or refused (the booking
// stands) with messages saying why.
type BookingCancellationResponseBody struct {
	BookingRef string    `json:"carrier_booking_reference"`
	Status     string    `json:"status"`
	Messages   []Message `json:"messages,omitempty"`
}

// Intent implements Body.
func (*BookingCancellationBody) Intent() string { return BookingCancellation }

// Intent implements Body.
func (*BookingCancellationResponseBody) Intent() string { return BookingCancellationResponse }

// Validate implements Body.
func (b *BookingCancellationBody) Validate() error {
	return ref("carrier_booking_reference", b.BookingRef)
}

// Validate implements Body: a refusal says why.
func (b *BookingCancellationResponseBody) Validate() error {
	errs := []error{ref("carrier_booking_reference", b.BookingRef), validateMessages(b.Messages)}
	switch b.Status {
	case StatusCancelled:
	case StatusRefused:
		if len(b.Messages) == 0 {
			errs = append(errs, errors.New("a refusal has no messages"))
		}
	default:
		errs = append(errs, fmt.Errorf("status: %s is not cancelled or refused", logsafe.Quote(b.Status)))
	}
	return errors.Join(errs...)
}
