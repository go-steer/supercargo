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

// Package shipper is the shipper side of the demo (docs/DESIGN.md, section
// A.9).
package shipper

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
)

// Shipper sends bookings through its node and records the confirmed ones.
type Shipper struct {
	Node     *node.Node
	Bookings *store.Bookings
	Now      func() time.Time
}

// Book sends a booking request to operator and returns its answer. Only a
// "confirmed" answer is recorded as a booking; "rejected" and the reserved
// "alternative_proposed" are returned to the caller to decide on.
//
// Book is the deterministic send step. In the demo it runs only after the
// person at the CLI confirmed the draft; no model calls it.
func (s *Shipper) Book(ctx context.Context, operator string, req *intents.BookingRequestBody) (*intents.BookingConfirmationBody, error) {
	h := envelope.NewHeader(intents.BookingRequest, s.Node.Domain(), operator, s.Now())
	resp, err := s.Node.Exchange(ctx, h, req, intents.BookingConfirmation)
	if err != nil {
		return nil, err
	}
	conf, ok := resp.Body.(*intents.BookingConfirmationBody)
	if !ok {
		return nil, errors.New("shipper: response is not a booking confirmation")
	}
	if conf.Status == intents.StatusConfirmed {
		// Keyed by (operator, ref), and never overwritten: an operator
		// reusing a reference is an error, not a replacement.
		if err := s.Bookings.Add(store.Booking{
			Ref:          conf.BookingRef,
			Shipper:      s.Node.Domain(),
			Operator:     operator,
			Request:      *req,
			Confirmation: *conf,
			CreatedAt:    s.Now(),
		}); errors.Is(err, store.ErrExists) {
			return nil, fmt.Errorf("shipper: %s confirmed %s again: %w", operator, conf.BookingRef, err)
		} else if err != nil {
			return nil, fmt.Errorf("shipper: recording %s's confirmation of %s: %w", operator, conf.BookingRef, err)
		}
	}
	return conf, nil
}
