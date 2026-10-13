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
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/go-steer/supercargo/internal/ports"

	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
)

// Publishing is what the simulator needs from a Publisher.
type Publishing interface {
	Publish(b store.Booking, body intents.Body) error
}

// Simulator plays each confirmed booking's voyage on an accelerated clock
// and publishes its events (docs/DESIGN.md, A.8): per container gate-in
// and load, the vessel's departure, an estimated arrival, the arrival, then
// per container discharge and gate-out. A truck leg plays the same way:
// per container pick-up, the truck's departure, an estimated delivery, the
// arrival, then per container drop-off. Disrupt delays a voyage (or a
// truck) and raises an alert.
//
// Simulated time runs HoursPerSecond hours per real second from the moment
// a booking starts, beginning a little before its planned departure, so
// event times read like the voyage's own.
type Simulator struct {
	Publisher Publishing
	// HoursPerSecond is the clock speed (default 1: a 134h voyage plays out
	// in a little over two minutes).
	HoursPerSecond float64
	// MaxTimelines bounds the voyages in progress (default 1000). Bookings
	// are free to make, so they mustn't grow the simulator without limit.
	MaxTimelines int
	// Tick is how often the clock is checked (default 100ms).
	Tick time.Duration
	// StartDelay is the real time between Start and the voyage's clock
	// starting (default 2s): the booking's confirmation must reach the
	// shipper, which records it, before the first event does.
	StartDelay time.Duration
	// OnError learns about events that couldn't be published. Optional;
	// called with the simulator's lock held, so it must not call back in.
	OnError func(ref string, err error)
	// OnPublish learns about each event and alert published (the node's
	// log shows it). Optional; the same rules as OnError.
	OnPublish func(ref string, body intents.Body)
	// Shared puts every voyage on one clock: simulated time is the real
	// time of the first voyage's start (the anchor) plus HoursPerSecond
	// hours per real second since. Bookings planned on real dates then
	// play in their real order, across nodes whose first bookings come
	// together (a forwarder's legs, one after another). Without it, each
	// voyage's clock starts just before its own first step.
	Shared bool
	Now    func() time.Time
	anchor time.Time

	once    sync.Once
	stop    chan struct{}
	done    chan struct{}
	mu      sync.Mutex
	running bool
	stopped bool
	lines   map[string]*timeline
	seq     int
}

// timeline is one booking's voyage: its remaining steps, in simulated-time
// order, and the mapping from real to simulated time.
type timeline struct {
	booking   store.Booking
	realStart time.Time
	simStart  time.Time
	arrival   time.Time // the current estimate
	delayed   time.Duration
	arrived   bool
	begun     bool      // a step has fired
	holdUntil time.Time // no step fires before this real time
	steps     []step
}

type step struct {
	at time.Time // simulated time it fires
	// body builds the step's event for when it fires: at is the step's
	// (possibly moved) time, which actual events report.
	body func(id string, at time.Time) intents.Body
	// afterArrival marks steps that move when the arrival does.
	afterArrival bool
	arrive       bool // the ARRI ACT step
	estimate     bool // an ARRI EST step, pointless once arrived
}

// Step offsets from the planned departure and arrival.
const (
	gateInBefore    = 26 * time.Hour
	loadBefore      = 4 * time.Hour
	estimateAfter   = time.Hour
	dischargeAfter  = 6 * time.Hour
	gateOutAfter    = 30 * time.Hour
	simLeadIn       = 27 * time.Hour // the clock starts this long before departure
	maxDisruptDelay = 30 * 24 * time.Hour

	// A truck leg's steps, from its planned pickup and delivery.
	roadDepartAfter = 15 * time.Minute
	roadEstimate    = 30 * time.Minute
	roadDropAfter   = time.Hour
	roadLeadIn      = 2 * time.Hour
)

var (
	// ErrUnknownBooking reports a booking with no voyage in progress.
	ErrUnknownBooking = errors.New("operator: no voyage in progress for that booking")
	// ErrArrived reports a disruption after the vessel arrived.
	ErrArrived = errors.New("operator: the vessel has already arrived")
	// ErrTooManyVoyages reports a start past MaxTimelines.
	ErrTooManyVoyages = errors.New("operator: too many voyages in progress")
)

func (s *Simulator) init() {
	s.once.Do(func() {
		if s.HoursPerSecond <= 0 {
			s.HoursPerSecond = 1
		}
		if s.MaxTimelines <= 0 {
			s.MaxTimelines = 1000
		}
		if s.Tick <= 0 {
			s.Tick = 100 * time.Millisecond
		}
		if s.StartDelay <= 0 {
			s.StartDelay = 2 * time.Second
		}
		if s.Now == nil {
			s.Now = time.Now
		}
		s.lines = map[string]*timeline{}
		s.stop = make(chan struct{})
		s.done = make(chan struct{})
	})
}

// Active is the number of voyages in progress.
func (s *Simulator) Active() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, tl := range s.lines {
		if len(tl.steps) > 0 {
			n++
		}
	}
	return n
}

// Run advances every voyage until Close. Call it once, in its own
// goroutine.
func (s *Simulator) Run() {
	s.init()
	s.mu.Lock()
	if s.running || s.stopped {
		s.mu.Unlock()
		return
	}
	s.running = true
	s.mu.Unlock()
	defer close(s.done)
	t := time.NewTicker(s.Tick)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.Advance()
		}
	}
}

// Close stops Run and waits for it.
func (s *Simulator) Close() {
	s.init()
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	close(s.stop)
	running := s.running
	s.mu.Unlock()
	if running {
		<-s.done
	}
}

// Start begins b's voyage. Only confirmed bookings with a vessel and
// planned times have one.
func (s *Simulator) Start(b store.Booking) error {
	s.init()
	if b.Road != nil {
		tl, err := s.roadTimeline(b)
		if err != nil {
			return err
		}
		return s.add(tl)
	}
	c := b.Confirmation
	if c.Status != intents.StatusConfirmed || c.EBL == nil {
		return fmt.Errorf("operator: booking %s is not confirmed", b.Ref)
	}
	dep, err1 := time.Parse(envelope.TimeLayout, c.PlannedDeparture)
	arr, err2 := time.Parse(envelope.TimeLayout, c.PlannedArrival)
	if err := errors.Join(err1, err2); err != nil {
		return fmt.Errorf("operator: booking %s: %w", b.Ref, err)
	}
	origin, dest := ports.Location(b.Request.PortOfLoading), ports.Location(b.Request.PortOfDischarge)
	vessel, voyage := c.EBL.VesselName, c.EBL.VoyageNumber
	equipment := func(code string, loc intents.Location, when time.Time, ref string) step {
		return step{at: when, body: func(id string, at time.Time) intents.Body {
			return &intents.TnTEventBody{EventID: id, EventType: intents.EventTypeEquipment, EventClassifierCode: intents.ClassifierActual,
				EventTypeCode: code, EventDateTime: stamp(at), UNLocationCode: loc.UNLocode, FacilityName: facility(loc),
				Description: describe("Container %s %s %s", ref, equipmentWords[code], facility(loc)), EquipmentReference: ref}
		}}
	}
	var steps []step
	for _, ref := range c.EquipmentRefs() {
		steps = append(steps, equipment("GTIN", origin, dep.Add(-gateInBefore), ref), equipment("LOAD", origin, dep.Add(-loadBefore), ref))
	}
	steps = append(steps,
		step{at: dep, body: func(id string, at time.Time) intents.Body {
			return &intents.TnTEventBody{EventID: id, EventType: intents.EventTypeTransport, EventClassifierCode: intents.ClassifierActual,
				EventTypeCode: "DEPA", EventDateTime: stamp(at), UNLocationCode: origin.UNLocode, FacilityName: facility(origin),
				Description: describe("%s %s departed %s", vessel, voyage, facility(origin)), VesselName: vessel, VoyageNumber: voyage}
		}},
	)
	tl := s.newTimeline(b, dep.Add(-simLeadIn), arr)
	if dep.Add(estimateAfter).Before(arr) { // a voyage that short has no time for an estimate
		steps = append(steps, step{at: dep.Add(estimateAfter), body: func(id string, _ time.Time) intents.Body { return s.estimate(id, tl, "") }})
	}
	steps = append(steps,
		step{at: arr, afterArrival: true, arrive: true, body: func(id string, at time.Time) intents.Body {
			return &intents.TnTEventBody{EventID: id, EventType: intents.EventTypeTransport, EventClassifierCode: intents.ClassifierActual,
				EventTypeCode: "ARRI", EventDateTime: stamp(at), UNLocationCode: dest.UNLocode, FacilityName: facility(dest),
				Description: describe("%s %s arrived at %s", vessel, voyage, facility(dest)), VesselName: vessel, VoyageNumber: voyage}
		}},
	)
	for _, ref := range c.EquipmentRefs() {
		d := equipment("DISC", dest, arr.Add(dischargeAfter), ref)
		g := equipment("GTOT", dest, arr.Add(gateOutAfter), ref)
		d.afterArrival, g.afterArrival = true, true
		steps = append(steps, d, g)
	}
	sort.SliceStable(steps, func(i, j int) bool { return steps[i].at.Before(steps[j].at) })
	tl.steps = steps
	return s.add(tl)
}

// newTimeline is a timeline on the simulator's clock: its own, starting
// at simStart StartDelay from now, or the shared one (Shared). Either
// way nothing fires for StartDelay, so the booking's confirmation reaches
// its shipper before the first event does.
func (s *Simulator) newTimeline(b store.Booking, simStart, arrival time.Time) *timeline {
	now := s.Now()
	tl := &timeline{booking: b, realStart: now.Add(s.StartDelay), simStart: simStart, arrival: arrival, holdUntil: now.Add(s.StartDelay)}
	if s.Shared {
		s.mu.Lock()
		if s.anchor.IsZero() {
			s.anchor = now
		}
		tl.realStart, tl.simStart = s.anchor, s.anchor
		s.mu.Unlock()
	}
	return tl
}

// add puts a new timeline in play.
func (s *Simulator) add(tl *timeline) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.lines[tl.booking.Ref]; ok {
		return fmt.Errorf("operator: booking %s already has a voyage", tl.booking.Ref)
	}
	if len(s.lines) >= s.MaxTimelines {
		return ErrTooManyVoyages
	}
	s.lines[tl.booking.Ref] = tl
	return nil
}

// roadTimeline is a truck leg's: per container pick-up at the pickup, the
// truck's departure, an estimated delivery, the arrival at the delivery,
// then per container drop-off.
func (s *Simulator) roadTimeline(b store.Booking) (*timeline, error) {
	c, r := b.Road.Confirmation, b.Road.Request
	if c.Status != intents.StatusConfirmed {
		return nil, fmt.Errorf("operator: booking %s is not confirmed", b.Ref)
	}
	pickup, err1 := time.Parse(envelope.TimeLayout, c.PlannedPickup)
	delivery, err2 := time.Parse(envelope.TimeLayout, c.PlannedDelivery)
	if err := errors.Join(err1, err2); err != nil {
		return nil, fmt.Errorf("operator: booking %s: %w", b.Ref, err)
	}
	from, to := r.Pickup, r.Delivery
	equipment := func(code string, stop intents.Stop, when time.Time, ref string, after bool) step {
		return step{at: when, afterArrival: after, body: func(id string, at time.Time) intents.Body {
			return &intents.TnTEventBody{EventID: id, EventType: intents.EventTypeEquipment, EventClassifierCode: intents.ClassifierActual,
				EventTypeCode: code, EventDateTime: stamp(at), UNLocationCode: stop.UNLocationCode, FacilityName: clip(stop.Facility, 128),
				Description: describe("Container %s %s %s", ref, equipmentWords[code], stop.Facility), EquipmentReference: ref}
		}}
	}
	tl := s.newTimeline(b, pickup.Add(-roadLeadIn), delivery)
	var steps []step
	refs := b.EquipmentRefs()
	for _, ref := range refs {
		steps = append(steps, equipment("PICK", from, pickup, ref, false))
	}
	// A leg planned shorter than the departure's offset still departs
	// before it arrives.
	depart := pickup.Add(roadDepartAfter)
	if delivery.Before(depart) {
		depart = delivery
	}
	steps = append(steps, step{at: depart, body: func(id string, at time.Time) intents.Body {
		return &intents.TnTEventBody{EventID: id, EventType: intents.EventTypeTransport, EventClassifierCode: intents.ClassifierActual,
			EventTypeCode: "DEPA", EventDateTime: stamp(at), UNLocationCode: from.UNLocationCode, FacilityName: clip(from.Facility, 128),
			Description: describe("Truck departed %s for %s", from.Facility, to.Facility)}
	}})
	if pickup.Add(roadEstimate).Before(delivery) {
		steps = append(steps, step{at: pickup.Add(roadEstimate), estimate: true, body: func(id string, _ time.Time) intents.Body { return s.estimate(id, tl, "") }})
	}
	steps = append(steps, step{at: delivery, afterArrival: true, arrive: true, body: func(id string, at time.Time) intents.Body {
		return &intents.TnTEventBody{EventID: id, EventType: intents.EventTypeTransport, EventClassifierCode: intents.ClassifierActual,
			EventTypeCode: "ARRI", EventDateTime: stamp(at), UNLocationCode: to.UNLocationCode, FacilityName: clip(to.Facility, 128),
			Description: describe("Truck arrived at %s", to.Facility)}
	}})
	for _, ref := range refs {
		steps = append(steps, equipment("DROP", to, delivery.Add(roadDropAfter), ref, true))
	}
	sort.SliceStable(steps, func(i, j int) bool { return steps[i].at.Before(steps[j].at) })
	tl.steps = steps
	return tl, nil
}

// Replan follows a truck leg's amended plan. Before anything has happened
// on the leg, it plays again from its new plan. Once it has begun (a
// container picked up), only the delivery and what follows move, and a new
// estimate says so. Either way a delay already announced (Disrupt) stays
// on top of the new plan: the shipper was told of it, and nothing says
// it's over. A finished leg stays finished: ErrUnknownBooking.
func (s *Simulator) Replan(b store.Booking) error {
	s.init()
	if b.Road == nil {
		return fmt.Errorf("operator: booking %s is not a truck leg", b.Ref)
	}
	fresh, err := s.roadTimeline(b)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tl, ok := s.lines[b.Ref]
	switch {
	case !ok:
		return ErrUnknownBooking
	case !tl.begun:
		fresh.shift(tl.delayed)
		s.lines[b.Ref] = fresh
		return nil
	case tl.arrived:
		return ErrArrived
	}
	tl.booking = b
	if d := fresh.arrival.Add(tl.delayed).Sub(tl.arrival); d != 0 {
		tl.shift(d)
		tl.delayed -= d // a new plan, not a delay
		// A delivery moved earlier than a step still to come (the estimate)
		// mustn't fire after it.
		sort.SliceStable(tl.steps, func(i, j int) bool { return tl.steps[i].at.Before(tl.steps[j].at) })
		_ = s.publish(tl, s.estimate(s.nextID("EV"), tl, "new delivery plan"))
	}
	return nil
}

// estimate is the ARRI EST event for tl's current arrival estimate; why,
// if set, says what moved it. It is called with s.mu held.
func (s *Simulator) estimate(id string, tl *timeline, why string) intents.Body {
	if r := tl.booking.Road; r != nil {
		to := r.Request.Delivery
		desc := "Truck expected at " + to.Facility
		if why != "" {
			desc += ": " + why
		}
		return &intents.TnTEventBody{EventID: id, EventType: intents.EventTypeTransport, EventClassifierCode: intents.ClassifierEstimated,
			EventTypeCode: "ARRI", EventDateTime: stamp(tl.arrival), UNLocationCode: to.UNLocationCode, FacilityName: clip(to.Facility, 128),
			Description: clip(desc, 280)}
	}
	c := tl.booking.Confirmation
	dest := ports.Location(tl.booking.Request.PortOfDischarge)
	desc := fmt.Sprintf("%s %s expected at %s", c.EBL.VesselName, c.EBL.VoyageNumber, facility(dest))
	if why != "" {
		desc += ": " + why
	}
	desc = clip(desc, 280)
	return &intents.TnTEventBody{EventID: id, EventType: intents.EventTypeTransport, EventClassifierCode: intents.ClassifierEstimated,
		EventTypeCode: "ARRI", EventDateTime: stamp(tl.arrival), UNLocationCode: dest.UNLocode, FacilityName: facility(dest),
		Description: desc, VesselName: c.EBL.VesselName, VoyageNumber: c.EBL.VoyageNumber}
}

// facility names a port in events: "Port of Rotterdam", within the
// event's 128-character limit (a place name comes from the booking).
func facility(loc intents.Location) string { return clip("Port of "+loc.Name, 128) }

// describe is an event description within its 280-character limit.
func describe(format string, a ...any) string { return clip(fmt.Sprintf(format, a...), 280) }

// clip cuts s to n characters, marking the cut.
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

// equipmentWords describe equipment events in their descriptions.
var equipmentWords = map[string]string{"GTIN": "gated in at", "LOAD": "loaded at", "DISC": "discharged at", "GTOT": "gated out at",
	"PICK": "picked up at", "DROP": "dropped off at"}

// Advance publishes every step that is due. Run calls it on each tick;
// tests call it directly.
func (s *Simulator) Advance() {
	s.init()
	now := s.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	// Publishing under the lock keeps a booking's events in order with a
	// concurrent Disrupt; Publish only queues, so it doesn't block.
	for ref, tl := range s.lines {
		if now.Before(tl.holdUntil) {
			continue
		}
		sim := tl.simNow(now, s.HoursPerSecond)
		for len(tl.steps) > 0 && !tl.steps[0].at.After(sim) {
			st := tl.steps[0]
			tl.steps = tl.steps[1:]
			if st.arrive {
				tl.arrived = true
			}
			tl.begun = true
			if st.estimate && tl.arrived {
				continue // a re-plan brought the arrival before it
			}
			// A failure is reported to OnError; the voyage goes on.
			_ = s.publish(tl, st.body(s.nextID("EV"), st.at))
		}
		if len(tl.steps) == 0 {
			delete(s.lines, ref)
		}
	}
}

func (tl *timeline) simNow(now time.Time, hoursPerSecond float64) time.Time {
	elapsed := now.Sub(tl.realStart)
	return tl.simStart.Add(time.Duration(float64(elapsed) * hoursPerSecond * 3600))
}

// Disrupt delays ref's arrival by delay, for reason: it publishes a new
// estimated arrival, then a delay alert citing it, and moves every later
// step. The alert is returned.
func (s *Simulator) Disrupt(ref string, delay time.Duration, reason string) (*intents.TnTAlertBody, error) {
	s.init()
	if delay <= 0 || delay > maxDisruptDelay {
		return nil, fmt.Errorf("operator: a delay must be positive and at most %v", maxDisruptDelay)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tl, ok := s.lines[ref]
	if !ok {
		return nil, ErrUnknownBooking
	}
	return s.disrupt(tl, delay, reason)
}

// VoyageAlert is one booking's outcome in a voyage's disruption: its
// alert, or why it got none. Applied: the delay took effect (its new
// estimate is out) even though the alert didn't, so disrupting again
// would delay it twice.
type VoyageAlert struct {
	BookingRef string                `json:"bookingRef"`
	Alert      *intents.TnTAlertBody `json:"alert,omitempty"`
	Applied    bool                  `json:"applied,omitempty"`
	Error      string                `json:"error,omitempty"`
}

// ErrUnknownVoyage: no booking not yet arrived is on that voyage.
var ErrUnknownVoyage = errors.New("operator: no booking under way on that voyage")

// ErrAlertNotSent: the delay is applied and its estimate published, but
// the alert wasn't. Don't disrupt again: that would delay it twice.
var ErrAlertNotSent = errors.New("operator: the delay is applied, but its alert was not sent (don't disrupt again)")

// DisruptVoyage delays every booking on voyage (of vessel, if not empty)
// that hasn't arrived, as Disrupt does each: one decision, a signed alert
// to each booking's subscriber. A booking that can't take the delay is
// reported and the rest go ahead; the outcomes are in booking order.
func (s *Simulator) DisruptVoyage(vessel, voyage string, delay time.Duration, reason string) ([]VoyageAlert, error) {
	s.init()
	if delay <= 0 || delay > maxDisruptDelay {
		return nil, fmt.Errorf("operator: a delay must be positive and at most %v", maxDisruptDelay)
	}
	if strings.TrimSpace(voyage) == "" {
		return nil, errors.New("operator: which voyage?")
	}
	// The reason is every alert's: refused once, before anything changes.
	probe := intents.TnTAlertBody{AlertID: "AL-0", Kind: intents.AlertDelay, Severity: severity(delay), Summary: reason,
		PreviousETA: "2026-01-01T00:00:00Z", NewETA: "2026-01-02T00:00:00Z", CauseEventIDs: []string{"EV-0"}}
	if err := probe.Validate(); err != nil {
		return nil, fmt.Errorf("operator: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var refs []string
	for ref, tl := range s.lines {
		v, n := tl.booking.Confirmation.Vessel()
		if n == voyage && (vessel == "" || v == vessel) && !tl.arrived {
			refs = append(refs, ref)
		}
	}
	if len(refs) == 0 {
		return nil, ErrUnknownVoyage
	}
	sort.Strings(refs)
	out := make([]VoyageAlert, 0, len(refs))
	for _, ref := range refs {
		a := VoyageAlert{BookingRef: ref}
		alert, err := s.disrupt(s.lines[ref], delay, reason)
		if err != nil {
			a.Error, a.Applied = err.Error(), errors.Is(err, ErrAlertNotSent)
		}
		a.Alert = alert
		out = append(out, a)
	}
	return out, nil
}

// disrupt delays one timeline; s.mu is held.
func (s *Simulator) disrupt(tl *timeline, delay time.Duration, reason string) (*intents.TnTAlertBody, error) {
	if tl.arrived {
		return nil, ErrArrived
	}
	if tl.delayed+delay > maxDisruptDelay {
		return nil, fmt.Errorf("operator: this voyage is already %v late; the total delay is at most %v", tl.delayed, maxDisruptDelay)
	}
	previous := tl.arrival
	alert := &intents.TnTAlertBody{
		Kind: intents.AlertDelay, Severity: severity(delay), Summary: reason,
		PreviousETA: stamp(previous), NewETA: stamp(previous.Add(delay)),
	}
	// Checked before anything changes: a reason that isn't valid alert
	// text is refused, not half-applied.
	alert.AlertID, alert.CauseEventIDs = "AL-0", []string{"EV-0"}
	if err := alert.Validate(); err != nil {
		return nil, fmt.Errorf("operator: %w", err)
	}
	tl.shift(delay)
	// A push recorded but not queued (ErrNotQueued) still happened: the
	// subscriber catches up by asking. Anything else recorded nothing.
	estID := s.nextID("EV")
	if err := s.publish(tl, s.estimate(estID, tl, reason)); err != nil && !errors.Is(err, ErrNotQueued) {
		tl.shift(-delay) // nothing was said, so nothing changed
		return nil, err
	}
	alert.AlertID, alert.CauseEventIDs = s.nextID("AL"), []string{estID}
	if err := s.publish(tl, alert); err != nil && !errors.Is(err, ErrNotQueued) {
		// The new estimate is out, so the delay stands; only the alert is
		// missing. Retrying would delay the voyage again.
		return nil, fmt.Errorf("%w: estimate %s published, but not the alert: %w", ErrAlertNotSent, estID, err)
	}
	return alert, nil
}

// shift moves the arrival, and every step after it, by d.
func (tl *timeline) shift(d time.Duration) {
	tl.arrival = tl.arrival.Add(d)
	tl.delayed += d
	for i := range tl.steps {
		if tl.steps[i].afterArrival {
			tl.steps[i].at = tl.steps[i].at.Add(d)
		}
	}
}

// severity grades a delay.
func severity(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return intents.SeverityHigh
	case d >= 12*time.Hour:
		return intents.SeverityMedium
	}
	return intents.SeverityLow
}

func (s *Simulator) publish(tl *timeline, body intents.Body) error {
	err := s.Publisher.Publish(tl.booking, body)
	switch {
	case err != nil && s.OnError != nil:
		s.OnError(tl.booking.Ref, err)
	case err == nil && s.OnPublish != nil:
		s.OnPublish(tl.booking.Ref, body)
	}
	return err
}

// nextID is a fresh event or alert ID. It is called with s.mu held.
func (s *Simulator) nextID(prefix string) string {
	s.seq++
	return fmt.Sprintf("%s-%06d", prefix, s.seq)
}

func stamp(t time.Time) string { return t.UTC().Format(envelope.TimeLayout) }
