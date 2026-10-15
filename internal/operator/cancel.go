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

package operator

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/intents"
)

// ErrUnderWay reports a shipment whose cargo has moved: it can no longer
// be cancelled.
var ErrUnderWay = errors.New("operator: the shipment is under way")

// CancellationService handles ulp.booking.cancellation: the shipper of an
// ocean booking or a truck leg withdraws it (docs/DESIGN.md, A.5 "Booking
// cancellation"). It is cancelled until its cargo has moved, and once
// cancelled it answers so again.
type CancellationService struct {
	Bookings *store.Bookings
	Log      *store.EventLog // the carrier's published events
	Now      func() time.Time
	// Stop, if set, ends the booking's shipment before it is stored as
	// cancelled, or returns ErrUnderWay if it has begun (the simulator's
	// Cancel).
	Stop func(ref string) error
	// Exclusive, if set, runs a step with nothing published meanwhile (the
	// publisher's Exclusive): the check that nothing moved and the
	// storing are then one step.
	Exclusive func(func() error) error
	// OnCancelled runs for each booking cancelled. Optional; it must not
	// block.
	OnCancelled func(store.Booking)
}

// Authorize implements node.Handler: the sender is the booking's shipper
// (A.6). The booking is looked up under this carrier, and an unknown
// booking, a forwarding order and another shipper's booking get the same
// answer.
func (s *CancellationService) Authorize(_ context.Context, req *node.Request) error {
	h := req.Envelope.Header
	b, err := s.Bookings.Get(h.Receiver, *h.BookingRef)
	switch {
	case err != nil:
		return &node.Rejection{Code: forbiddenLeg.Code, Detail: forbiddenLeg.Detail, Reason: "no booking " + *h.BookingRef}
	case b.Order != nil:
		return &node.Rejection{Code: forbiddenLeg.Code, Detail: forbiddenLeg.Detail, Reason: *h.BookingRef + " is a forwarding order"}
	case b.Shipper != h.Sender:
		return &node.Rejection{Code: forbiddenLeg.Code, Detail: forbiddenLeg.Detail, Reason: *h.BookingRef + " is " + b.Shipper + "'s, not " + h.Sender + "'s"}
	}
	return nil
}

// Handle implements node.Handler.
func (s *CancellationService) Handle(_ context.Context, req *node.Request) (*node.Reply, error) {
	h := req.Envelope.Header
	ref := req.Body.(*intents.BookingCancellationBody).BookingRef
	answer := func(status string, msgs ...intents.Message) *node.Reply {
		return &node.Reply{BookingRef: ref, Body: &intents.BookingCancellationResponseBody{BookingRef: ref, Status: status, Messages: msgs}}
	}
	underWay := intents.Message{Code: "under_way", Detail: "The cargo has moved; the booking stands."}
	b, err := s.Bookings.Get(h.Receiver, ref)
	if err != nil {
		return nil, err // authorized a moment ago; gone only on a reset
	}
	if b.Cancelled() {
		return answer(intents.StatusCancelled), nil // the same answer again
	}
	if moved(s.Log.List(h.Receiver, ref)) {
		return answer(intents.StatusRefused, underWay), nil
	}
	if s.Stop != nil {
		if err := s.Stop(ref); errors.Is(err, ErrUnderWay) {
			return answer(intents.StatusRefused, underWay), nil
		} else if err != nil {
			return nil, err
		}
	}
	exclusive := s.Exclusive
	if exclusive == nil {
		exclusive = func(fn func() error) error { return fn() }
	}
	at := s.Now()
	var stored store.Booking
	changed, under := false, false
	err = exclusive(func() error {
		// Again, now that nothing can be published: an event recorded
		// since the first look means the cargo moved after all.
		if moved(s.Log.List(h.Receiver, ref)) {
			under = true
			return nil
		}
		var err error
		stored, err = s.Bookings.Update(h.Receiver, ref, func(b *store.Booking) error {
			if b.CancelledAt.IsZero() {
				b.CancelledAt, changed = at, true
			}
			return nil
		})
		return err
	})
	switch {
	case err != nil:
		return nil, err
	case under:
		return answer(intents.StatusRefused, underWay), nil
	}
	if changed && s.OnCancelled != nil {
		s.OnCancelled(stored)
	}
	return answer(intents.StatusCancelled), nil
}

// moved reports whether recs hold an actual event: the cargo has moved.
func moved(recs []store.Record) bool {
	for _, r := range recs {
		var e intents.TnTEventBody
		if r.Intent == intents.TnTEvent && json.Unmarshal(r.Body, &e) == nil && e.EventClassifierCode == intents.ClassifierActual {
			return true
		}
	}
	return false
}
