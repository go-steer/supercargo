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
	"sort"
	"sync"
	"time"

	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
)

// Publishing is what the relay needs from the operator side's publisher:
// record an event or alert for one of its bookings (an order), and push
// it to the order's subscriber.
type Publishing interface {
	Publish(b store.Booking, body intents.Body) error
}

// Amending is what a confirmed proposal needs from the shipper side.
type Amending interface {
	AmendRoad(ctx context.Context, carrier, ref string, a *intents.RoadBookingAmendmentBody) (*intents.RoadBookingConfirmationBody, error)
}

// Relay tells an order's shipper one story from its legs
// (docs/proposals/door-to-door-forwarder.md): each event a leg's carrier
// signs is re-issued as the order's event, saying where it came from
// (derived_from); a leg's delay is projected onto the order's delivery
// and announced as the order's alert; and when it moves a later truck,
// a proposal to amend that leg waits for a person to confirm it.
//
// It is deterministic: a carrier's free text travels only as the event's
// description it signed; the order's alerts say what the forwarder
// computed, in its own words.
type Relay struct {
	Orders  *Orders
	Publish Publishing
	Amend   Amending
	Now     func() time.Time
	// OnProposal learns of each proposal made. Optional; it must not
	// block.
	OnProposal func(Proposal)

	mu        sync.Mutex
	seq       int
	relayed   map[legEvent]string // a leg's event or alert → the order's
	proposals map[string]*Proposal
}

type legEvent struct{ carrier, ref, id string }

// Proposal is a change a leg's delay calls for: amend a later truck leg's
// windows to the cargo's new ready time. Nothing is sent until a person
// confirms it.
type Proposal struct {
	ID        string                           `json:"id"`
	OrderRef  string                           `json:"orderRef"`
	Leg       int                              `json:"leg"` // 1-based, as in the plan
	Carrier   string                           `json:"carrier"`
	LegRef    string                           `json:"legRef"`
	Amendment intents.RoadBookingAmendmentBody `json:"amendment"`
	Why       string                           `json:"why"`
	Status    string                           `json:"status"` // proposed, confirmed, refused, failed
	Outcome   string                           `json:"outcome,omitempty"`
}

// Proposal states.
const (
	ProposalOpen      = "proposed"
	ProposalSending   = "sending"
	ProposalConfirmed = "confirmed"
	ProposalRefused   = "refused"
	ProposalFailed    = "failed"
)

// MaxProposals bounds the proposals kept.
const MaxProposals = 1000

var (
	// ErrUnknownProposal is a proposal this forwarder doesn't hold.
	ErrUnknownProposal = errors.New("forwarder: no such proposal")
	// ErrProposalClosed is a proposal confirmed or refused already.
	ErrProposalClosed = errors.New("forwarder: the proposal is closed")
)

// orderOf finds the order a leg belongs to, and the leg's index in it.
func (r *Relay) orderOf(leg store.Booking) (store.Booking, int, bool) {
	self := r.Orders.Node.Domain()
	for _, b := range r.Orders.Bookings.List() {
		if b.Order == nil || b.Operator != self {
			continue
		}
		for i, l := range b.Order.Legs {
			if l.Carrier == leg.Operator && l.Ref == leg.Ref {
				return b, i, true
			}
		}
	}
	return store.Booking{}, 0, false
}

func (r *Relay) nextID(prefix string) string {
	r.seq++
	return fmt.Sprintf("%s-%s-%06d", r.Orders.RefPrefix, prefix, r.seq)
}

// Event re-issues a leg's event as its order's, with the leg's event as
// its signed source. A leg that isn't an order's is no business of the
// relay's.
func (r *Relay) Event(leg store.Booking, e *intents.TnTEventBody) error {
	order, i, ok := r.orderOf(leg)
	if !ok {
		return nil
	}
	l := order.Order.Legs[i]
	out := *e
	out.DerivedFrom = []intents.Provenance{{ID: e.EventID, Signed: true}}
	out.Description = clip(fmt.Sprintf("%s (leg %d, %s)", e.Description, i+1, l.Name), 280)
	r.mu.Lock()
	defer r.mu.Unlock()
	out.EventID = r.nextID("EV")
	if err := r.Publish.Publish(order, &out); err != nil {
		return fmt.Errorf("forwarder: relaying %s's %s on %s: %w", leg.Operator, e.EventID, order.Ref, err)
	}
	r.remember(legEvent{leg.Operator, leg.Ref, e.EventID}, out.EventID)
	return nil
}

func (r *Relay) remember(k legEvent, id string) {
	if r.relayed == nil {
		r.relayed = map[legEvent]string{}
	}
	if len(r.relayed) >= 100_000 {
		clear(r.relayed) // bounded: an alert citing a forgotten event isn't relayed
	}
	r.relayed[k] = id
}

// Alert projects a leg's delay onto its order. A delay of a leg with
// trucks after it moves the cargo's ready time for the next one; if that
// truck would now collect too early, the order's delivery moves by as
// much, the order's shipper gets the order's delay alert (citing the
// order's re-issued events the leg's alert cited, derived from the leg's
// alert), and a proposal to move the truck waits for a person. A delay
// of the last leg moves the delivery directly. Anything else (an early
// arrival, an exception, a delay absorbed by the slack) is re-issued as
// is, without a projection.
func (r *Relay) Alert(leg store.Booking, a *intents.TnTAlertBody) error {
	order, i, ok := r.orderOf(leg)
	if !ok {
		return nil
	}
	legs := order.Order.Legs
	l := legs[i]
	r.mu.Lock()
	defer r.mu.Unlock()
	var causes []string
	for _, c := range a.CauseEventIDs {
		if id, ok := r.relayed[legEvent{leg.Operator, leg.Ref, c}]; ok {
			causes = append(causes, id)
		}
	}
	if len(causes) == 0 {
		// The shipper would refuse an alert citing events it doesn't hold.
		return fmt.Errorf("forwarder: %s's alert %s on %s cites no event relayed", leg.Operator, a.AlertID, order.Ref)
	}
	out := &intents.TnTAlertBody{Kind: a.Kind, Severity: a.Severity, CauseEventIDs: causes,
		DerivedFrom: []intents.Provenance{{ID: a.AlertID, Signed: true}}}
	newETA, ok := parseTime(a.NewETA)
	// The delivery as planned now: the last leg's arrival (which a
	// confirmed re-timing updates), not the first answer's.
	delivery, hasDelivery := parseTime(legs[len(legs)-1].Arrival)
	var proposal *Proposal
	switch {
	case a.Kind != intents.AlertDelay || !ok || !hasDelivery:
		out.Summary = clip(fmt.Sprintf("Leg %d (%s) reports %s.", i+1, l.Name, kindWords[a.Kind]), 280)
		out.PreviousETA, out.NewETA = a.PreviousETA, a.NewETA
	case i == len(legs)-1 && newETA.After(delivery):
		// The last leg: its arrival is the delivery.
		out.Summary = clip(fmt.Sprintf("Leg %d (%s) is delayed; the delivery moves with it.", i+1, l.Name), 280)
		out.PreviousETA, out.NewETA = stamp(delivery), stamp(newETA)
	case i == len(legs)-1:
		out.Summary = clip(fmt.Sprintf("Leg %d (%s) is delayed; the planned delivery still holds.", i+1, l.Name), 280)
		out.Kind, out.Severity = intents.AlertException, intents.SeverityLow
		out.PreviousETA, out.NewETA = "", ""
	default:
		next := legs[i+1]
		ready := afterLeg(store.LegRef{Mode: l.Mode, Arrival: stamp(newETA)}, newETA)
		pickup, ok1 := parseTime(next.Departure)
		arrive, ok2 := parseTime(next.Arrival)
		if !ok1 || !ok2 || !ready.After(pickup) || next.Mode != intents.ModeRoad {
			out.Summary = clip(fmt.Sprintf("Leg %d (%s) is delayed; the plan absorbs it.", i+1, l.Name), 280)
			out.Kind, out.Severity = intents.AlertException, intents.SeverityLow
			out.PreviousETA, out.NewETA = "", ""
			break
		}
		shift := ready.Sub(pickup)
		out.Summary = clip(fmt.Sprintf("Leg %d (%s) is delayed by %s; the delivery is expected later, and leg %d is to be re-timed.",
			i+1, l.Name, hours(shift), i+2), 280)
		out.PreviousETA, out.NewETA = stamp(delivery), stamp(delivery.Add(shift))
		proposal = &Proposal{OrderRef: order.Ref, Leg: i + 2, Carrier: next.Carrier, LegRef: next.Ref, Status: ProposalOpen,
			Amendment: intents.RoadBookingAmendmentBody{BookingRef: next.Ref,
				Pickup:   &intents.Window{Earliest: stamp(ready)},
				Delivery: &intents.Window{Latest: stamp(arrive.Add(shift))}},
			Why: clip(fmt.Sprintf("Leg %d (%s) arrives %s later than leg %d planned: collect from %s.", i+1, l.Name, hours(shift), i+2, stamp(ready)), 280)}
	}
	out.AlertID = r.nextID("AL")
	if err := r.Publish.Publish(order, out); err != nil {
		return fmt.Errorf("forwarder: relaying %s's %s on %s: %w", leg.Operator, a.AlertID, order.Ref, err)
	}
	r.remember(legEvent{leg.Operator, leg.Ref, a.AlertID}, out.AlertID)
	if proposal != nil {
		if r.proposals == nil {
			r.proposals = map[string]*Proposal{}
		}
		if len(r.proposals) < MaxProposals {
			proposal.ID = r.nextID("PR")
			r.proposals[proposal.ID] = proposal
			if r.OnProposal != nil {
				r.OnProposal(*proposal)
			}
		}
	}
	return nil
}

// Proposals are an order's proposals, oldest first.
func (r *Relay) Proposals(orderRef string) []Proposal {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []Proposal{}
	for _, p := range r.proposals {
		if p.OrderRef == orderRef {
			out = append(out, *p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Confirm sends a proposal's amendment to the leg's carrier: the step a
// person at the forwarder confirms; no model calls it. A confirmed new
// plan replaces the leg's in the order's record; a refusal leaves it, and
// either way the proposal closes with the outcome.
func (r *Relay) Confirm(ctx context.Context, id string) (Proposal, error) {
	r.mu.Lock()
	p, ok := r.proposals[id]
	if !ok {
		r.mu.Unlock()
		return Proposal{}, fmt.Errorf("%w: %s", ErrUnknownProposal, id)
	}
	if p.Status != ProposalOpen {
		r.mu.Unlock()
		return Proposal{}, fmt.Errorf("%w: %s is %s", ErrProposalClosed, id, p.Status)
	}
	p.Status = ProposalSending // no second confirmation while this one is out
	a := p.Amendment
	r.mu.Unlock()

	conf, err := r.Amend.AmendRoad(ctx, p.Carrier, p.LegRef, &a)
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case err != nil:
		p.Status, p.Outcome = ProposalFailed, err.Error()
	case conf.Status != intents.StatusConfirmed:
		p.Status, p.Outcome = ProposalRefused, "the carrier kept its plan"
	default:
		p.Status, p.Outcome = ProposalConfirmed, "collect "+conf.PlannedPickup+", deliver "+conf.PlannedDelivery
		self := r.Orders.Node.Domain()
		_, uerr := r.Orders.Bookings.Update(self, p.OrderRef, func(b *store.Booking) error {
			if b.Order == nil || p.Leg-1 >= len(b.Order.Legs) || b.Order.Legs[p.Leg-1].Ref != p.LegRef {
				return errors.New("forwarder: the order's legs changed meanwhile")
			}
			b.Order.Legs[p.Leg-1].Departure, b.Order.Legs[p.Leg-1].Arrival = conf.PlannedPickup, conf.PlannedDelivery
			return nil
		})
		if uerr != nil {
			p.Outcome += " (not recorded in the order: " + uerr.Error() + ")"
		}
	}
	return *p, err
}

func stamp(t time.Time) string { return t.UTC().Format(envelope.TimeLayout) }

// hours says a duration in whole hours, rounded up: "48 h".
func hours(d time.Duration) string { return fmt.Sprintf("%d h", int((d+time.Hour-1)/time.Hour)) }

// kindWords name an alert's kind in the order's alert.
var kindWords = map[string]string{intents.AlertDelay: "a delay", intents.AlertEarly: "an early arrival", intents.AlertException: "an exception"}
