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

package forwarder

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-steer/supercargo/internal/config"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
)

// Step is one leg of an order's plan: who carries it, by what mode, from
// where to where, and the facilities it hands over at.
type Step struct {
	Mode         string `json:"mode"`
	Carrier      string `json:"carrier"`
	Name         string `json:"name"`
	From         string `json:"from"`
	To           string `json:"to"`
	FromFacility string `json:"fromFacility"`
	ToFacility   string `json:"toFacility"`
	EDI          bool   `json:"edi,omitempty"`
	// Booked is the leg's booking reference once booked.
	Booked string `json:"booked,omitempty"`
}

// LegBooker sends the legs' bookings: the forwarder's shipper side.
type LegBooker interface {
	Book(ctx context.Context, operator string, req *intents.BookingRequestBody) (*intents.BookingConfirmationBody, error)
	BookRoad(ctx context.Context, carrier string, req *intents.RoadBookingRequestBody) (*intents.RoadBookingConfirmationBody, error)
}

// Orchestrator plans an order on its lane and, once a person confirms the
// plan, books each leg in turn and answers the order with the journey
// (docs/proposals/door-to-door-forwarder.md). It is deterministic code: a
// model may later help draft a plan, but booking and answering are only
// ever this, run by a person's confirmation.
type Orchestrator struct {
	Orders *Orders
	Legs   LegBooker
	Lanes  []config.Lane
	// Name is the forwarder's company name, the legs' shipper; Endpoint
	// is where the legs' carriers push their events (the forwarder's own
	// /ulp).
	Name     string
	Endpoint string
	Now      func() time.Time

	mu      sync.Mutex
	booking map[string]bool // orders being booked
}

// Handover is how long after a ship's planned arrival a container is
// ready for the truck: discharged, cleared and released.
const Handover = 30 * time.Hour

var (
	// ErrNoLane is an order on a lane the forwarder doesn't run.
	ErrNoLane = errors.New("forwarder: no lane for this order")
	// ErrEDILeg is a lane with a leg booked over EDI, which this node
	// can't book yet.
	ErrEDILeg = errors.New("forwarder: the lane has an EDI leg, which this node can't book yet")
	// ErrLegRefused is a leg its carrier didn't confirm.
	ErrLegRefused = errors.New("forwarder: a carrier didn't confirm its leg")
)

// Plan is the order's plan: the steps of its lane, with the legs booked
// so far.
func (o *Orchestrator) Plan(ref string) ([]Step, error) {
	b, err := o.Orders.Bookings.Get(o.Orders.Node.Domain(), ref)
	if err != nil || b.Order == nil {
		return nil, fmt.Errorf("%w: %s", ErrUnknownOrder, ref)
	}
	return o.plan(b)
}

func (o *Orchestrator) plan(b store.Booking) ([]Step, error) {
	req := b.Order.Request
	from, to := req.PlaceOfReceipt.UNLocationCode, req.PlaceOfDelivery.UNLocationCode
	for _, l := range o.Lanes {
		if l.From != from || l.To != to {
			continue
		}
		steps := make([]Step, len(l.Legs))
		at, facility := from, placeName(req.PlaceOfReceipt)
		for i, leg := range l.Legs {
			s := Step{Mode: leg.Mode, Carrier: leg.Carrier, Name: leg.Name, From: at, To: leg.To, FromFacility: facility, ToFacility: leg.Facility, EDI: leg.EDI}
			if i == len(l.Legs)-1 {
				s.ToFacility = placeName(req.PlaceOfDelivery)
			}
			if i < len(b.Order.Legs) {
				s.Booked = b.Order.Legs[i].Ref
			}
			steps[i] = s
			at, facility = leg.To, leg.Facility
		}
		return steps, nil
	}
	return nil, fmt.Errorf("%w: %s → %s", ErrNoLane, from, to)
}

// placeName names a place for a truck's stop: its facility, or its street.
func placeName(p intents.Place) string {
	if p.Facility != "" {
		return p.Facility
	}
	if p.Address != nil {
		return p.Address.Street + ", " + p.Address.Locality
	}
	return p.UNLocationCode
}

// Book books each leg of a pending order's plan that isn't booked yet, in
// journey order, recording each as it goes, then answers the order with
// the journey. It is the step a person at the forwarder confirms. If a
// carrier refuses its leg, Book stops: the legs booked so far stay
// recorded, and the person decides (book again later, which resumes after
// them, or answer the order with a rejection).
func (o *Orchestrator) Book(ctx context.Context, ref string) (*intents.ForwardingConfirmationBody, error) {
	o.mu.Lock()
	if o.booking[ref] {
		o.mu.Unlock()
		return nil, fmt.Errorf("%w: %s is being booked", ErrAnswered, ref)
	}
	if o.booking == nil {
		o.booking = map[string]bool{}
	}
	o.booking[ref] = true
	o.mu.Unlock()
	defer func() {
		o.mu.Lock()
		delete(o.booking, ref)
		o.mu.Unlock()
	}()

	self := o.Orders.Node.Domain()
	b, err := o.Orders.Bookings.Get(self, ref)
	if err != nil || b.Order == nil {
		return nil, fmt.Errorf("%w: %s", ErrUnknownOrder, ref)
	}
	if b.Order.Confirmation.Status != intents.StatusPending {
		return nil, fmt.Errorf("%w: %s is %s", ErrAnswered, ref, b.Order.Confirmation.Status)
	}
	steps, err := o.plan(b)
	if err != nil {
		return nil, err
	}
	for i, s := range steps {
		if s.EDI && i >= len(b.Order.Legs) {
			return nil, fmt.Errorf("%w (leg %d, %s)", ErrEDILeg, i+1, s.Name)
		}
	}
	req := b.Order.Request
	ready := o.Now().UTC()
	if t, ok := parseTime(req.PlaceOfReceipt.Earliest); ok && t.After(ready) {
		ready = t
	}
	var containers []string
	for i, s := range steps {
		if i < len(b.Order.Legs) {
			// Booked before (a resumed booking): carry on from its plan.
			done := b.Order.Legs[i]
			ready, containers = afterLeg(done, ready), done.Containers
			continue
		}
		leg, err := o.bookLeg(ctx, b, i, s, ready, containers, i == len(steps)-1)
		if err != nil {
			return nil, err
		}
		if _, err := o.Orders.Bookings.Update(self, ref, func(b *store.Booking) error {
			if b.Order == nil || len(b.Order.Legs) != i {
				return errors.New("forwarder: the order's legs changed meanwhile")
			}
			b.Order.Legs = append(b.Order.Legs, *leg)
			return nil
		}); err != nil {
			return nil, err
		}
		b.Order.Legs = append(b.Order.Legs, *leg)
		ready, containers = afterLeg(*leg, ready), leg.Containers
	}
	conf := &intents.ForwardingConfirmationBody{Status: intents.StatusConfirmed}
	for _, l := range b.Order.Legs {
		conf.Legs = append(conf.Legs, intents.PlannedLeg{Mode: l.Mode, From: l.From, To: l.To, Carrier: l.Name, Vessel: l.Vessel,
			PlannedDeparture: l.Departure, PlannedArrival: l.Arrival})
	}
	conf.PlannedDelivery = b.Order.Legs[len(b.Order.Legs)-1].Arrival
	if err := o.Orders.Answer(ctx, ref, conf); err != nil {
		return nil, err
	}
	return conf, nil
}

// afterLeg is when the cargo is ready for the next leg: a truck's
// delivery, or a ship's arrival plus the handover.
func afterLeg(l store.LegRef, ready time.Time) time.Time {
	t, ok := parseTime(l.Arrival)
	if !ok {
		return ready
	}
	if l.Mode == intents.ModeOcean {
		t = t.Add(Handover)
	}
	return t
}

// bookLeg books one leg with its carrier, as the leg's shipper.
func (o *Orchestrator) bookLeg(ctx context.Context, b store.Booking, i int, s Step, ready time.Time, containers []string, last bool) (*store.LegRef, error) {
	req := b.Order.Request
	shipperRef := fmt.Sprintf("%s-L%d", b.Ref, i+1)
	sub := &intents.Subscription{Endpoint: o.Endpoint, Intents: []string{intents.TnTEvent, intents.TnTAlert}}
	leg := &store.LegRef{Mode: s.Mode, Carrier: s.Carrier, Name: s.Name, From: s.From, To: s.To}
	switch s.Mode {
	case intents.ModeOcean:
		conf, err := o.Legs.Book(ctx, s.Carrier, &intents.BookingRequestBody{
			ShipperRef: shipperRef, ShipperName: o.Name, PortOfLoading: s.From, PortOfDischarge: s.To,
			Equipment: req.Equipment, Cargo: req.Cargo, EarliestDeparture: ready.Format(intents.DateLayout), Subscription: sub,
		})
		if err != nil {
			return nil, fmt.Errorf("forwarder: leg %d with %s: %w", i+1, s.Name, err)
		}
		if conf.Status != intents.StatusConfirmed {
			return nil, fmt.Errorf("%w: leg %d, %s answered %s", ErrLegRefused, i+1, s.Name, conf.Status)
		}
		vessel, voyage := conf.Vessel()
		leg.Ref, leg.Departure, leg.Arrival, leg.Containers = conf.BookingRef, conf.PlannedDeparture, conf.PlannedArrival, conf.EquipmentRefs()
		if vessel != "" {
			leg.Vessel = vessel + " " + voyage
		}
	case intents.ModeRoad:
		r := &intents.RoadBookingRequestBody{
			ShipperRef: shipperRef,
			Shipper:    intents.Party{Name: o.Name, Domain: o.Orders.Node.Domain()},
			Pickup:     intents.Stop{Facility: s.FromFacility, UNLocationCode: s.From, Earliest: ready.Format(envelope.TimeLayout)},
			Delivery:   intents.Stop{Facility: s.ToFacility, UNLocationCode: s.To},
			Equipment:  req.Equipment, Cargo: req.Cargo, Subscription: sub,
		}
		if i == 0 {
			r.Pickup.Address = req.PlaceOfReceipt.Address
		}
		if last {
			consignee := req.Consignee
			r.Consignee = &consignee
			r.Delivery.Address, r.Delivery.Latest = req.PlaceOfDelivery.Address, req.PlaceOfDelivery.Latest
		}
		if len(containers) <= totalContainers(req.Equipment) {
			r.EquipmentReferences = containers
		}
		conf, err := o.Legs.BookRoad(ctx, s.Carrier, r)
		if err != nil {
			return nil, fmt.Errorf("forwarder: leg %d with %s: %w", i+1, s.Name, err)
		}
		if conf.Status != intents.StatusConfirmed {
			return nil, fmt.Errorf("%w: leg %d, %s answered %s", ErrLegRefused, i+1, s.Name, conf.Status)
		}
		leg.Ref, leg.Departure, leg.Arrival, leg.Containers = conf.BookingRef, conf.PlannedPickup, conf.PlannedDelivery, containers
	default:
		return nil, fmt.Errorf("forwarder: leg %d: mode %q", i+1, s.Mode) // config validated
	}
	return leg, nil
}

func totalContainers(eq []intents.Equipment) int {
	n := 0
	for _, e := range eq {
		n += e.Count
	}
	return n
}

func parseTime(s string) (time.Time, bool) {
	t, err := time.Parse(envelope.TimeLayout, s)
	return t, err == nil
}
