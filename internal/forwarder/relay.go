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
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/go-steer/supercargo/internal/edi"
	"github.com/go-steer/supercargo/internal/shipper"
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
	statuses  map[statusKey]bool  // EDI statuses re-issued
	legs      map[string]int      // a re-issued event's or alert's ID → its leg (0-based)
	order     []legEvent          // relayed's keys, oldest first
	proposals map[string]*Proposal
}

// MaxDelay is the largest delay projected onto an order; a leg's alert
// claiming more is passed on as an exception, without a projection.
const MaxDelay = 60 * 24 * time.Hour

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
	// ProposalSuperseded: a later delay of the same leg made a newer one.
	ProposalSuperseded = "superseded"
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
	// The leg's part always shows, however long the carrier's text.
	suffix := fmt.Sprintf(" (leg %d, %s)", i+1, clip(l.Name, 128))
	out.Description = clip(e.Description, 280-len([]rune(suffix))) + suffix
	r.mu.Lock()
	defer r.mu.Unlock()
	out.EventID = r.nextID("EV")
	if err := r.Publish.Publish(order, &out); err != nil {
		return fmt.Errorf("forwarder: relaying %s's %s on %s: %w", leg.Operator, e.EventID, order.Ref, err)
	}
	r.remember(legEvent{leg.Operator, leg.Ref, e.EventID}, out.EventID)
	r.noteLeg(out.EventID, i)
	return nil
}

// MaxRelayed bounds the leg events remembered for alerts to cite; the
// oldest are forgotten first.
const MaxRelayed = 100_000

// Unsigned re-issues a status a carrier without a node reported over EDI
// as its order's event, marked derived_from an unsigned source (the EDI
// message, by its sender and the carrier's consignment number). Its
// description is the forwarder's own, from the status's meaning; a status
// whose code the mapping doesn't know isn't re-issued.
func (r *Relay) Unsigned(carrier string, s edi.Status) error {
	order, i, ok := r.orderOf(store.Booking{Operator: carrier, Ref: s.Reference})
	if !ok {
		return fmt.Errorf("%w: %s's status for %s", ErrNoLeg, carrier, s.Reference)
	}
	l := order.Order.Legs[i]
	if d := s.Time.Sub(r.Now()); d > StatusWindow || d < -StatusWindow {
		return fmt.Errorf("%w: %s's status %s on %s is dated %s", ErrSkipped, carrier, s.Code, s.Reference, stamp(s.Time))
	}
	// A container from an unsigned source is named only if it's one of the
	// order's own (from its signed legs); another customer's never is.
	equipment := ""
	for _, leg := range order.Order.Legs {
		if slices.Contains(leg.Containers, s.Equipment) && !leg.Unsigned {
			equipment = s.Equipment
		}
	}
	s.Equipment = equipment
	at, end := l.From, l.To
	e := intents.TnTEventBody{EventClassifierCode: intents.ClassifierActual, EventDateTime: stamp(s.Time), FacilityName: s.Facility}
	switch s.Meaning {
	case edi.PickedUp:
		e.EventType, e.EventTypeCode, e.UNLocationCode = intents.EventTypeTransport, "DEPA", at
		if s.Equipment != "" {
			e.EventType, e.EventTypeCode, e.EquipmentReference = intents.EventTypeEquipment, "PICK", s.Equipment
		}
	case edi.Departed:
		e.EventType, e.EventTypeCode, e.UNLocationCode = intents.EventTypeTransport, "DEPA", at
	case edi.Arrived:
		e.EventType, e.EventTypeCode, e.UNLocationCode = intents.EventTypeTransport, "ARRI", end
	case edi.GateIn, edi.Delivered:
		e.EventType, e.EventTypeCode, e.UNLocationCode = intents.EventTypeTransport, "ARRI", end
		if s.Equipment != "" {
			code := map[edi.Meaning]string{edi.GateIn: "GTIN", edi.Delivered: "DROP"}[s.Meaning]
			e.EventType, e.EventTypeCode, e.EquipmentReference = intents.EventTypeEquipment, code, s.Equipment
		}
	default:
		return fmt.Errorf("%w: %s's status code %q on %s means nothing the forwarder knows", ErrSkipped, carrier, s.Code, s.Reference)
	}
	// A place the status names, if it's one of its leg's own: a carrier
	// without a node can't put its statuses at another leg's port.
	if s.UNLocode == at || s.UNLocode == end {
		e.UNLocationCode = s.UNLocode
	}
	e.Description = clip(fmt.Sprintf("%s (leg %d, %s, reported over EDI)", meaningWords[s.Meaning], i+1, clip(l.Name, 128)), 280)
	src := intents.Provenance{Source: clip("EDIFACT IFTSTA from "+s.Sender, 128), Signed: false}
	if refOK(s.CarrierRef) {
		src.Reference = s.CarrierRef
	}
	e.DerivedFrom = []intents.Provenance{src}
	r.mu.Lock()
	defer r.mu.Unlock()
	// The same status again (a file dropped twice): re-issued once.
	k := statusKey{carrier, s.Reference, s.Code, stamp(s.Time)}
	if r.statuses[k] {
		return fmt.Errorf("%w: %s's status %s on %s was re-issued already", ErrSkipped, carrier, s.Code, s.Reference)
	}
	if r.statuses == nil || len(r.statuses) >= MaxRelayed {
		r.statuses = map[statusKey]bool{}
	}
	e.EventID = r.nextID("EV")
	if err := r.Publish.Publish(order, &e); err != nil {
		return fmt.Errorf("forwarder: re-issuing %s's status on %s: %w", carrier, order.Ref, err)
	}
	r.statuses[k] = true
	r.noteLeg(e.EventID, i)
	return nil
}

var (
	// ErrNoLeg is a status for a reference that is no order's leg (yet:
	// a carrier may report before its acknowledgement's leg is recorded).
	ErrNoLeg = errors.New("forwarder: no order's leg has that reference")
	// ErrSkipped is a status not re-issued: a code the mapping doesn't
	// know, one re-issued already, or one dated far off.
	ErrSkipped = errors.New("forwarder: status not re-issued")
)

// StatusWindow is how far from now a status may be dated and still be
// re-issued (statuses run on an accelerated clock in the demo).
const StatusWindow = 400 * 24 * time.Hour

type statusKey struct{ carrier, ref, code, at string }

// meaningWords say what an EDI status means, in the forwarder's words.
var meaningWords = map[edi.Meaning]string{edi.PickedUp: "Picked up", edi.Departed: "Departed", edi.Arrived: "Arrived",
	edi.GateIn: "Gated in", edi.Delivered: "Delivered"}

// refOK is a reference ULP bodies accept: 1-64 of [A-Za-z0-9_-], from a
// letter or digit.
func refOK(s string) bool {
	if s == "" || len(s) > 64 || !isAlnum(s[0]) {
		return false
	}
	for i := range len(s) {
		if c := s[i]; !isAlnum(c) && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

func isAlnum(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
}

// noteLeg keeps which leg a re-issued record came from (at most
// MaxRelayed; the map starts over past that). r.mu is held.
func (r *Relay) noteLeg(id string, leg int) {
	if r.legs == nil || len(r.legs) >= MaxRelayed {
		r.legs = map[string]int{}
	}
	r.legs[id] = leg
}

// LegOf is the leg (1-based) a record the relay re-issued came from, as
// the relay knew when it re-issued it; false for one it didn't, or has
// forgotten.
func (r *Relay) LegOf(id string) (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	leg, ok := r.legs[id]
	return leg + 1, ok
}

func (r *Relay) remember(k legEvent, id string) {
	if r.relayed == nil {
		r.relayed = map[legEvent]string{}
	}
	if _, ok := r.relayed[k]; !ok {
		r.order = append(r.order, k)
	}
	r.relayed[k] = id
	for len(r.order) > MaxRelayed {
		delete(r.relayed, r.order[0])
		r.order = r.order[1:]
	}
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
	prevETA, _ := parseTime(a.PreviousETA)
	switch {
	case a.Kind == intents.AlertDelay && ok && newETA.Sub(prevETA) > MaxDelay:
		out.Summary = clip(fmt.Sprintf("Leg %d (%s) reports a delay of over %s; the forwarder is checking it.", i+1, l.Name, hours(MaxDelay)), 280)
		out.Kind, out.PreviousETA, out.NewETA = intents.AlertException, "", ""
	case a.Kind != intents.AlertDelay || !ok || !hasDelivery:
		out.Summary = clip(fmt.Sprintf("Leg %d (%s) reports %s.", i+1, l.Name, kindWords[a.Kind]), 280)
		out.PreviousETA, out.NewETA = "", ""
		if i == len(legs)-1 && a.Kind != intents.AlertException {
			// The last leg's times are the delivery's.
			out.PreviousETA, out.NewETA = a.PreviousETA, a.NewETA
		} else if a.Kind != intents.AlertException {
			out.Kind = intents.AlertException // a middle leg's times aren't the order's
		}
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
		out.Summary = clip(fmt.Sprintf("Leg %d (%s) is delayed; the delivery moves by %s, and leg %d is to be re-timed.",
			i+1, l.Name, hours(shift), i+2), 280)
		out.PreviousETA, out.NewETA = stamp(delivery), stamp(delivery.Add(shift))
		why := fmt.Sprintf("Leg %d (%s) arrives %s later than leg %d planned: collect from %s.", i+1, l.Name, hours(shift), i+2, stamp(ready))
		amendment := intents.RoadBookingAmendmentBody{BookingRef: next.Ref, Pickup: &intents.Window{Earliest: stamp(ready)}}
		newArrival := arrive.Add(shift)
		deadline, hasDeadline := parseTime(order.Order.Request.PlaceOfDelivery.Latest)
		switch {
		case i+1 < len(legs)-1 || !hasDeadline:
			// A middle truck, or no deadline: its delivery window follows.
			amendment.Delivery = &intents.Window{Latest: stamp(newArrival)}
		case newArrival.After(deadline):
			// The last truck can't make the consignee's date any more: the
			// person decides, knowing it.
			amendment.Delivery = &intents.Window{Latest: stamp(newArrival)}
			why += " This misses the order's delivery date (" + order.Order.Request.PlaceOfDelivery.Latest + ")."
		}
		// Otherwise the last truck keeps the order's deadline as it is.
		proposal = &Proposal{OrderRef: order.Ref, Leg: i + 2, Carrier: next.Carrier, LegRef: next.Ref, Status: ProposalOpen,
			Amendment: amendment, Why: clip(why, 280)}
	}
	out.AlertID = r.nextID("AL")
	if err := r.Publish.Publish(order, out); err != nil {
		return fmt.Errorf("forwarder: relaying %s's %s on %s: %w", leg.Operator, a.AlertID, order.Ref, err)
	}
	r.remember(legEvent{leg.Operator, leg.Ref, a.AlertID}, out.AlertID)
	r.noteLeg(out.AlertID, i)
	if proposal != nil {
		return r.propose(proposal)
	}
	return nil
}

// propose keeps a new proposal: it supersedes the leg's open ones (only
// the latest projection may be confirmed), closed ones make room, and at
// most MaxProposals are open. r.mu is held.
func (r *Relay) propose(p *Proposal) error {
	if r.proposals == nil {
		r.proposals = map[string]*Proposal{}
	}
	open := 0
	for id, q := range r.proposals {
		switch {
		case q.OrderRef == p.OrderRef && q.Leg == p.Leg && q.Status == ProposalOpen:
			q.Status, q.Outcome = ProposalSuperseded, "a later delay replaced it"
		case q.Status == ProposalOpen || q.Status == ProposalSending:
			open++
			continue
		}
		if len(r.proposals) >= MaxProposals {
			delete(r.proposals, id) // closed: make room
		}
	}
	if open >= MaxProposals {
		return fmt.Errorf("forwarder: %d proposals open; none made for %s leg %d", open, p.OrderRef, p.Leg)
	}
	p.ID = r.nextID("PR")
	r.proposals[p.ID] = p
	if r.OnProposal != nil {
		r.OnProposal(*p)
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
// plan replaces the leg's in the order's record; a refusal leaves it and
// closes the proposal. One that couldn't be sent stays open, to retry.
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
	case errors.Is(err, shipper.ErrPlanMismatch), errors.Is(err, shipper.ErrLegChanged):
		// The carrier answered, but its plan wasn't recorded: it may hold
		// the new one. Closed; the person checks with the carrier.
		p.Status, p.Outcome = ProposalFailed, err.Error()+" (the carrier may hold the new plan: check with them)"
	case err != nil:
		// Not sent, or refused before any plan: open again, to retry.
		p.Status, p.Outcome = ProposalOpen, "not sent: "+err.Error()
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
