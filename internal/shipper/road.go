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
	"reflect"
	"time"

	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
)

// ErrPlanMismatch: a road carrier's plan doesn't fit what was asked for
// (a pickup outside its window, an amendment answered for another leg).
// It isn't recorded, and the person sees why. The carrier may hold it
// all the same: until ULP can cancel a leg, the person takes it up with
// the carrier.
var ErrPlanMismatch = errors.New("shipper: the carrier's plan contradicts the request")

// BookRoad sends a road booking request to carrier and returns its answer.
// A confirmed leg is recorded, as Book records an ocean booking, after its
// plan is held to the request's windows.
//
// Like Book, it is a deterministic send step: it runs only after a person
// confirmed the leg; no model calls it.
func (s *Shipper) BookRoad(ctx context.Context, carrier string, req *intents.RoadBookingRequestBody) (*intents.RoadBookingConfirmationBody, error) {
	if s.NoBooking {
		return nil, ErrNoShipperRole
	}
	h := envelope.NewHeader(intents.RoadBookingRequest, s.Node.Domain(), carrier, s.Now())
	resp, err := s.Node.Exchange(ctx, h, req, intents.RoadBookingConfirmation)
	if err != nil {
		return nil, err
	}
	conf, ok := resp.Body.(*intents.RoadBookingConfirmationBody)
	if !ok {
		return nil, errors.New("shipper: response is not a road booking confirmation")
	}
	if conf.Status != intents.StatusConfirmed {
		return conf, nil
	}
	if err := checkPlan(req, conf); err != nil {
		return nil, fmt.Errorf("%w: %s's plan for %s: %w", ErrPlanMismatch, carrier, conf.BookingRef, err)
	}
	if err := s.Bookings.Add(store.Booking{
		Ref: conf.BookingRef, Shipper: s.Node.Domain(), Operator: carrier,
		Road:      &store.RoadBooking{Request: *req, Confirmation: *conf},
		CreatedAt: s.Now(),
	}); errors.Is(err, store.ErrExists) {
		return nil, fmt.Errorf("shipper: %s confirmed %s again: %w", carrier, conf.BookingRef, err)
	} else if err != nil {
		return nil, fmt.Errorf("shipper: recording %s's confirmation of %s: %w", carrier, conf.BookingRef, err)
	}
	return conf, nil
}

// AmendRoad sends an amendment of one of our truck legs to its carrier.
// A confirmed new plan replaces the recorded one (the windows and the
// plan); a rejection leaves the leg as it was, and is returned for the
// person to see.
func (s *Shipper) AmendRoad(ctx context.Context, carrier, ref string, a *intents.RoadBookingAmendmentBody) (*intents.RoadBookingConfirmationBody, error) {
	b, err := s.Bookings.Get(carrier, ref)
	if err != nil || b.Road == nil {
		return nil, fmt.Errorf("%w: truck leg %s with %s", ErrNotOurBooking, ref, carrier)
	}
	a.BookingRef = ref
	h := envelope.NewHeader(intents.RoadBookingAmendment, s.Node.Domain(), carrier, s.Now())
	h.BookingRef = &ref
	resp, err := s.Node.Exchange(ctx, h, a, intents.RoadBookingConfirmation)
	if err != nil {
		return nil, err
	}
	conf, ok := resp.Body.(*intents.RoadBookingConfirmationBody)
	if !ok {
		return nil, errors.New("shipper: response is not a road booking confirmation")
	}
	if conf.BookingRef != ref {
		// The node checks the reply answers our message, not which leg it
		// names: a confirmation of another leg isn't this one's.
		return nil, fmt.Errorf("%w: %s answered an amendment of %s for %s", ErrPlanMismatch, carrier, ref, conf.BookingRef)
	}
	if conf.Status != intents.StatusConfirmed {
		return conf, nil
	}
	amended := b.Road.Request
	amendWindow(&amended.Pickup, a.Pickup)
	amendWindow(&amended.Delivery, a.Delivery)
	if err := checkPlan(&amended, conf); err != nil {
		return nil, fmt.Errorf("%w: %s's new plan for %s: %w", ErrPlanMismatch, carrier, ref, err)
	}
	if conf.SubscriptionID != b.Road.Confirmation.SubscriptionID {
		return nil, fmt.Errorf("%w: %s changed %s's subscription", ErrPlanMismatch, carrier, ref)
	}
	snapshot := b.Road
	if _, err := s.Bookings.Update(carrier, ref, func(b *store.Booking) error {
		// Only over the leg this amendment was made from: if another
		// amendment was recorded meanwhile, which plan the carrier holds
		// now depends on the order it took them in, which we don't know.
		if !reflect.DeepEqual(b.Road, snapshot) {
			return ErrLegChanged
		}
		b.Road.Request, b.Road.Confirmation = amended, *conf
		return nil
	}); err != nil {
		return nil, fmt.Errorf("shipper: recording %s's new plan for %s: %w", carrier, ref, err)
	}
	return conf, nil
}

// ErrLegChanged: another amendment of the leg was recorded while this one
// was out. The carrier confirmed this one, but the leg's record is the
// other's; the person checks with the carrier (an inquiry, or the leg's
// events) before acting on either plan.
var ErrLegChanged = errors.New("shipper: the leg changed while the amendment was out")

// checkPlan holds a carrier's confirmed plan to the leg asked for: the
// subscription asked for, and a pickup and delivery inside their windows.
func checkPlan(req *intents.RoadBookingRequestBody, conf *intents.RoadBookingConfirmationBody) error {
	var errs []error
	if req.Subscription != nil && conf.SubscriptionID == "" {
		errs = append(errs, errors.New("no subscription_id for the subscription asked for"))
	}
	for _, c := range []struct {
		field, at string
		stop      intents.Stop
	}{{"planned_pickup", conf.PlannedPickup, req.Pickup}, {"planned_delivery", conf.PlannedDelivery, req.Delivery}} {
		t, _ := time.Parse(envelope.TimeLayout, c.at) // validated
		if e, err := time.Parse(envelope.TimeLayout, c.stop.Earliest); err == nil && t.Before(e) {
			errs = append(errs, fmt.Errorf("%s %s is before the window opens (%s)", c.field, c.at, c.stop.Earliest))
		}
		if l, err := time.Parse(envelope.TimeLayout, c.stop.Latest); err == nil && t.After(l) {
			errs = append(errs, fmt.Errorf("%s %s is after the window closes (%s)", c.field, c.at, c.stop.Latest))
		}
	}
	return errors.Join(errs...)
}

// amendWindow puts an amendment's window into a stop, as the carrier does:
// each time given replaces the stop's; one left out stays.
func amendWindow(s *intents.Stop, w *intents.Window) {
	if w == nil {
		return
	}
	if w.Earliest != "" {
		s.Earliest = w.Earliest
	}
	if w.Latest != "" {
		s.Latest = w.Latest
	}
}
