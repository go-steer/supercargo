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
	"strings"
	"time"

	"github.com/go-steer/supercargo/internal/config"
	"github.com/go-steer/supercargo/internal/edi"
	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/internal/shipper"
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

// LegCanceller cancels the legs' bookings: the forwarder's shipper side.
type LegCanceller interface {
	CancelLeg(ctx context.Context, carrier, ref string) (*intents.BookingCancellationResponseBody, error)
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
	// EDI books the legs of carriers without a node (nil: such a lane
	// can't be booked).
	EDI *Gateway
	// Cancel cancels a leg booked over ULP (nil: such a leg can't be
	// cancelled, so an order with one can't be rejected).
	Cancel LegCanceller
	// Name is the forwarder's company name, the legs' shipper; Endpoint
	// is where the legs' carriers push their events (the forwarder's own
	// /ulp).
	Name     string
	Endpoint string
	Now      func() time.Time
}

// Handover is how long after a ship's planned arrival a container is
// ready for the truck: discharged, cleared and released.
const Handover = 30 * time.Hour

var (
	// ErrNoLane is an order on a lane the forwarder doesn't run.
	ErrNoLane = errors.New("forwarder: no lane for this order")
	// ErrEDILeg is a lane with a leg booked over EDI with a carrier this
	// node has no EDI file drop for.
	ErrEDILeg = errors.New("forwarder: the lane has an EDI leg this node can't book (no EDI partner configured)")
	// ErrLegRefused is a leg its carrier didn't confirm.
	ErrLegRefused = errors.New("forwarder: a carrier didn't confirm its leg")
	// ErrPlanUnfit is a leg whose carrier's plan the journey can't use (a
	// ship leaving before the cargo is ready), or a journey that can't
	// make the order's delivery date.
	ErrPlanUnfit = errors.New("forwarder: the journey doesn't fit the order")
	// ErrPlanChanged is an order whose booked legs no longer match its
	// lane (the lane was edited after they were booked).
	ErrPlanChanged = errors.New("forwarder: the order's booked legs don't match its lane")
	// ErrLegKept is a leg its carrier refused to cancel (its cargo has
	// moved): the carrier holds it.
	ErrLegKept = errors.New("forwarder: a carrier keeps its leg")
	// ErrCancelFailed is a leg that couldn't be cancelled (the carrier
	// unreachable, no way to cancel it here): try again.
	ErrCancelFailed = errors.New("forwarder: a leg couldn't be cancelled")
	// ErrCancelPending is a leg whose cancellation is out, unanswered:
	// it's neither booked nor off until the carrier answers. Rejecting
	// again waits for the answer.
	ErrCancelPending = errors.New("forwarder: a leg's cancellation is unanswered")
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
		// A door is reached by truck: a lane starting or ending at sea
		// can't serve a door at that end.
		if req.PlaceOfReceipt.Type == intents.PlaceDoor && l.Legs[0].Mode != intents.ModeRoad ||
			req.PlaceOfDelivery.Type == intents.PlaceDoor && l.Legs[len(l.Legs)-1].Mode != intents.ModeRoad {
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
	release, err := o.Orders.claim(ref)
	if err != nil {
		return nil, err
	}
	defer release()

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
		if s.EDI && i >= len(b.Order.Legs) && (o.EDI == nil || o.EDI.Partners[s.Carrier] == nil) {
			return nil, fmt.Errorf("%w (leg %d, %s)", ErrEDILeg, i+1, s.Name)
		}
	}
	req := b.Order.Request
	// available is when the cargo can go (the order's ready time, then
	// each leg's handover); ready, no earlier than now, is what new
	// bookings ask for. A leg booked before (an instruction sent again)
	// may leave between the two.
	var available time.Time
	if t, ok := parseTime(req.PlaceOfReceipt.Earliest); ok {
		available = t
	}
	ready := o.Now().UTC().Truncate(time.Second) // times on the wire are whole seconds
	if available.After(ready) {
		ready = available
	}
	latest, hasLatest := parseTime(req.PlaceOfDelivery.Latest)
	var containers []string
	for i, s := range steps {
		last := i == len(steps)-1
		if i < len(b.Order.Legs) {
			// Booked before (a resumed booking): carry on from its plan,
			// if it's still this step's.
			done := b.Order.Legs[i]
			if done.Mode != s.Mode || done.Carrier != s.Carrier || done.From != s.From || done.To != s.To {
				return nil, fmt.Errorf("%w: leg %d was booked with %s, %s → %s", ErrPlanChanged, i+1, done.Carrier, done.From, done.To)
			}
			if done.Unsigned && o.EDI != nil && o.EDI.Cancelling(done.Ref) {
				return nil, fmt.Errorf("%w: leg %d (%s): reject the order again to hear the carrier's answer", ErrCancelPending, i+1, done.Ref)
			}
			ready, containers = afterLeg(done, ready), done.Containers
			available = ready
			continue
		}
		if hasLatest && ready.After(latest) {
			return nil, fmt.Errorf("%w: the cargo is ready for leg %d only on %s, after the order's delivery date %s",
				ErrPlanUnfit, i+1, ready.Format(envelope.TimeLayout), req.PlaceOfDelivery.Latest)
		}
		leg, err := o.bookLeg(ctx, b, i, s, ready, containers, last)
		if err != nil {
			return nil, err
		}
		// The carrier's plan, held to the journey before it's used: a leg
		// that leaves before the cargo is ready, or (last) delivers late.
		// It isn't recorded, and it's withdrawn: the person confirmed
		// booking this journey, and a leg that doesn't fit it was never
		// part of that.
		if dep, ok := parseTime(leg.Departure); !ok || dep.Before(available) {
			return nil, o.withdraw(ctx, *leg, fmt.Errorf("%w: %s planned leg %d (%s) to leave %s, before the cargo is ready (%s)",
				ErrPlanUnfit, s.Name, i+1, leg.Ref, leg.Departure, available.Format(envelope.TimeLayout)))
		}
		if arr, ok := parseTime(leg.Arrival); last && hasLatest && (!ok || arr.After(latest)) {
			return nil, o.withdraw(ctx, *leg, fmt.Errorf("%w: %s planned leg %d (%s) to arrive %s, after the order's delivery date %s",
				ErrPlanUnfit, s.Name, i+1, leg.Ref, leg.Arrival, req.PlaceOfDelivery.Latest))
		}
		if _, err := o.Orders.Bookings.Update(self, ref, func(b *store.Booking) error {
			// Still pending (the claim keeps a hand-made answer out, and a
			// reset is the only other change), and still this leg's turn.
			if b.Order == nil || b.Order.Confirmation.Status != intents.StatusPending || len(b.Order.Legs) != i {
				return errors.New("forwarder: the order changed meanwhile")
			}
			b.Order.Legs = append(b.Order.Legs, *leg)
			return nil
		}); err != nil {
			return nil, err
		}
		b.Order.Legs = append(b.Order.Legs, *leg)
		ready, containers = afterLeg(*leg, ready), leg.Containers
		available = ready
	}
	conf := &intents.ForwardingConfirmationBody{Status: intents.StatusConfirmed}
	for _, l := range b.Order.Legs {
		conf.Legs = append(conf.Legs, intents.PlannedLeg{Mode: l.Mode, From: l.From, To: l.To, Carrier: l.Name, Vessel: l.Vessel,
			PlannedDeparture: l.Departure, PlannedArrival: l.Arrival})
	}
	conf.PlannedDelivery = b.Order.Legs[len(b.Order.Legs)-1].Arrival
	if err := o.Orders.answer(ctx, ref, conf); err != nil {
		return nil, err
	}
	return conf, nil
}

// withdraw cancels a leg booked but not recorded (its plan doesn't fit),
// and says so after why: withdrawn, or kept by its carrier.
func (o *Orchestrator) withdraw(ctx context.Context, leg store.LegRef, why error) error {
	if err := o.cancelLeg(ctx, leg); err != nil {
		return fmt.Errorf("%w; it couldn't be withdrawn, so take it up with the carrier: %w", why, err)
	}
	return fmt.Errorf("%w; withdrawn from the carrier", why)
}

// cancelLeg cancels one leg with its carrier: over EDI for a carrier
// without a node, else as a ULP booking cancellation. ErrLegKept if the
// carrier refused.
func (o *Orchestrator) cancelLeg(ctx context.Context, leg store.LegRef) error {
	if leg.Unsigned {
		if o.EDI == nil {
			return fmt.Errorf("%w: %s %s (no EDI gateway)", ErrCancelFailed, leg.Carrier, leg.Ref)
		}
		ack, err := o.EDI.Cancel(ctx, leg.Carrier, leg.Ref)
		if errors.Is(err, ErrNoAck) {
			return fmt.Errorf("%w: %s %s: %w", ErrCancelPending, leg.Carrier, leg.Ref, err)
		}
		if err != nil {
			return fmt.Errorf("%w: %s %s: %w", ErrCancelFailed, leg.Carrier, leg.Ref, err)
		}
		if !ack.Accepted {
			return fmt.Errorf("%w: %s refused to cancel %s (EDI errors %s)", ErrLegKept, leg.Name, leg.Ref, strings.Join(ack.Errors, ", "))
		}
		return nil
	}
	if o.Cancel == nil {
		return fmt.Errorf("%w: %s %s (no way to cancel a ULP leg here)", ErrCancelFailed, leg.Carrier, leg.Ref)
	}
	a, err := o.Cancel.CancelLeg(ctx, leg.Carrier, leg.Ref)
	if errors.Is(err, shipper.ErrCancelled) {
		return nil // cancelled before, its leg not yet off the order
	}
	if err != nil {
		var remote *node.RemoteError
		if errors.As(err, &remote) {
			// The carrier's own words stay out of what we show: its code.
			err = errors.New(remote.Code)
		}
		return fmt.Errorf("%w: %s %s: %w", ErrCancelFailed, leg.Carrier, leg.Ref, err)
	}
	if a.Status != intents.StatusCancelled {
		codes := make([]string, len(a.Messages))
		for n, m := range a.Messages {
			codes[n] = m.Code
		}
		return fmt.Errorf("%w: %s refused to cancel %s (%s)", ErrLegKept, leg.Name, leg.Ref, strings.Join(codes, ", "))
	}
	return nil
}

// Reject answers a pending order with a rejection, cancelling first the
// legs booked for it, last first. Each leg cancelled comes off the order
// (kept as withdrawn); if a carrier keeps its leg, Reject stops there:
// the order stays pending with the legs before it, and the person decides
// (booking again resumes after them). It is the step a person at the
// forwarder confirms; no model calls it.
func (o *Orchestrator) Reject(ctx context.Context, ref string, conf *intents.ForwardingConfirmationBody) error {
	if conf.Status != intents.StatusRejected {
		return fmt.Errorf("%w: Reject rejects", ErrInvalidAnswer)
	}
	release, err := o.Orders.claim(ref)
	if err != nil {
		return err
	}
	defer release()
	self := o.Orders.Node.Domain()
	b, err := o.Orders.Bookings.Get(self, ref)
	if err != nil || b.Order == nil {
		return fmt.Errorf("%w: %s", ErrUnknownOrder, ref)
	}
	if b.Order.Confirmation.Status != intents.StatusPending {
		return fmt.Errorf("%w: %s is %s", ErrAnswered, ref, b.Order.Confirmation.Status)
	}
	for i := len(b.Order.Legs) - 1; i >= 0; i-- {
		leg := b.Order.Legs[i]
		if err := o.cancelLeg(ctx, leg); err != nil {
			return fmt.Errorf("leg %d of %s: %w", i+1, ref, err)
		}
		if _, err := o.Orders.Bookings.Update(self, ref, func(b *store.Booking) error {
			// Still pending (the claim keeps a hand-made answer out, and a
			// reset is the only other change), and this still its last leg.
			if b.Order == nil || b.Order.Confirmation.Status != intents.StatusPending || len(b.Order.Legs) != i+1 {
				return errors.New("forwarder: the order changed meanwhile")
			}
			b.Order.Legs = b.Order.Legs[:i]
			b.Order.Withdrawn = append(b.Order.Withdrawn, leg)
			return nil
		}); err != nil {
			return err
		}
	}
	return o.Orders.answer(ctx, ref, conf)
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
	if s.EDI {
		return o.bookEDI(ctx, b, i, s, ready, shipperRef, leg)
	}
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
			leg.Vessel = clip(vessel+" "+voyage, 128) // a planned leg's vessel is at most 128
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

// clip cuts s to n characters.
func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// bookEDI instructs a carrier without a node over EDIFACT and waits for
// its acknowledgement. Its plan is the forwarder's own (an
// acknowledgement gives no times): collect when the cargo is ready, and
// arrive the partner's planned transit later. The leg is marked unsigned.
func (o *Orchestrator) bookEDI(ctx context.Context, b store.Booking, i int, s Step, ready time.Time, ref string, leg *store.LegRef) (*store.LegRef, error) {
	req := b.Order.Request
	p := o.EDI.Partners[s.Carrier]
	pickup := edi.Stop{Name: clip(s.FromFacility, 70), UNLocode: s.From, Country: s.From[:2], Earliest: ready}
	if a := req.PlaceOfReceipt.Address; i == 0 && a != nil {
		pickup.Street, pickup.Locality, pickup.PostalCode, pickup.Country = clip(a.Street, 70), clip(a.Locality, 70), a.PostalCode, a.Country
	}
	delivery := edi.Stop{Name: clip(s.ToFacility, 70), UNLocode: s.To, Country: s.To[:2]}
	var containers []edi.Container
	for _, e := range req.Equipment {
		containers = append(containers, edi.Container{SizeType: e.ISOType, Count: e.Count, GrossWeightKg: e.GrossWeightKg})
	}
	instruction := edi.Instruction{Reference: ref, ShipperRef: req.ShipperRef,
		Pickup: pickup, Delivery: delivery, Containers: containers,
		CargoDescription: clip(req.Cargo.Description, 70), HSCode: req.Cargo.HSCode}
	// A retried booking: the instruction already sent goes again, so the
	// carrier sees one instruction, not two (its pickup time was the ready
	// time then). One the carrier refused, or whose cancellation it
	// accepted, is sent anew under the next attempt's reference,
	// MF-0007-L1-2.
	for attempt := 2; ; attempt++ {
		sent, ack, ok := o.EDI.Instructed(instruction.Reference)
		if !ok {
			break
		}
		if ack != nil && ack.Accepted && o.EDI.Cancelling(instruction.Reference) && !o.EDI.Withdrawn(instruction.Reference) {
			return nil, fmt.Errorf("%w: leg %d (%s)", ErrCancelPending, i+1, instruction.Reference)
		}
		if ack == nil || ack.Accepted && !o.EDI.Withdrawn(instruction.Reference) {
			instruction = sent
			break
		}
		if attempt > 9 {
			return nil, fmt.Errorf("%w: leg %d, %s refused it %d times", ErrLegRefused, i+1, s.Name, attempt-1)
		}
		instruction.Reference = fmt.Sprintf("%s-%d", ref, attempt)
	}
	ack, err := o.EDI.Instruct(ctx, s.Carrier, instruction)
	if err != nil {
		return nil, fmt.Errorf("forwarder: leg %d with %s: %w", i+1, s.Name, err)
	}
	if !ack.Accepted {
		return nil, fmt.Errorf("%w: leg %d, %s didn't accept the instruction", ErrLegRefused, i+1, s.Name)
	}
	leg.Ref, leg.CarrierRef, leg.Documents, leg.Unsigned = instruction.Reference, ack.CarrierRef, ack.TransportDocuments, true
	pickupAt := instruction.Pickup.Earliest.UTC()
	leg.Departure, leg.Arrival = stamp(pickupAt), stamp(pickupAt.Add(p.Transit))
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
