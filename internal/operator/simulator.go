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
	"sync"
	"time"

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
// per container discharge and gate-out. Disrupt delays a voyage and raises
// an alert.
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
	Now       func() time.Time

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
	c := b.Confirmation
	if c.Status != intents.StatusConfirmed || c.Vessel == nil {
		return fmt.Errorf("operator: booking %s is not confirmed", b.Ref)
	}
	dep, err1 := time.Parse(envelope.TimeLayout, c.PlannedDeparture)
	arr, err2 := time.Parse(envelope.TimeLayout, c.PlannedArrival)
	if err := errors.Join(err1, err2); err != nil {
		return fmt.Errorf("operator: booking %s: %w", b.Ref, err)
	}
	origin, dest := b.Request.Origin, b.Request.Destination
	transport := &intents.Transport{Vessel: c.Vessel.Name, Voyage: c.Vessel.Voyage}
	equipment := func(code string, loc intents.Location, when time.Time, ref string) step {
		return step{at: when, body: func(id string, at time.Time) intents.Body {
			return &intents.TnTEventBody{EventID: id, EventType: intents.EventTypeEquipment, EventClassifierCode: intents.ClassifierActual,
				EventCode: code, EventDateTime: stamp(at), Location: loc, EquipmentRef: ref}
		}}
	}
	var steps []step
	for _, ref := range c.EquipmentRefs {
		steps = append(steps, equipment("GTIN", origin, dep.Add(-gateInBefore), ref), equipment("LOAD", origin, dep.Add(-loadBefore), ref))
	}
	steps = append(steps,
		step{at: dep, body: func(id string, at time.Time) intents.Body {
			return &intents.TnTEventBody{EventID: id, EventType: intents.EventTypeTransport, EventClassifierCode: intents.ClassifierActual,
				EventCode: "DEPA", EventDateTime: stamp(at), Location: origin, Transport: transport}
		}},
	)
	tl := &timeline{booking: b, realStart: s.Now().Add(s.StartDelay), simStart: dep.Add(-simLeadIn), arrival: arr}
	if dep.Add(estimateAfter).Before(arr) { // a voyage that short has no time for an estimate
		steps = append(steps, step{at: dep.Add(estimateAfter), body: func(id string, _ time.Time) intents.Body { return s.estimate(id, tl) }})
	}
	steps = append(steps,
		step{at: arr, afterArrival: true, arrive: true, body: func(id string, at time.Time) intents.Body {
			return &intents.TnTEventBody{EventID: id, EventType: intents.EventTypeTransport, EventClassifierCode: intents.ClassifierActual,
				EventCode: "ARRI", EventDateTime: stamp(at), Location: dest, Transport: transport}
		}},
	)
	for _, ref := range c.EquipmentRefs {
		d := equipment("DISC", dest, arr.Add(dischargeAfter), ref)
		g := equipment("GTOT", dest, arr.Add(gateOutAfter), ref)
		d.afterArrival, g.afterArrival = true, true
		steps = append(steps, d, g)
	}
	sort.SliceStable(steps, func(i, j int) bool { return steps[i].at.Before(steps[j].at) })
	tl.steps = steps

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.lines[b.Ref]; ok {
		return fmt.Errorf("operator: booking %s already has a voyage", b.Ref)
	}
	if len(s.lines) >= s.MaxTimelines {
		return ErrTooManyVoyages
	}
	s.lines[b.Ref] = tl
	return nil
}

// estimate is the ARRI EST event for tl's current arrival estimate. It is
// called with s.mu held.
func (s *Simulator) estimate(id string, tl *timeline) intents.Body {
	c := tl.booking.Confirmation
	return &intents.TnTEventBody{EventID: id, EventType: intents.EventTypeTransport, EventClassifierCode: intents.ClassifierEstimated,
		EventCode: "ARRI", EventDateTime: stamp(tl.arrival), Location: tl.booking.Request.Destination,
		Transport: &intents.Transport{Vessel: c.Vessel.Name, Voyage: c.Vessel.Voyage}}
}

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
		sim := tl.simNow(now, s.HoursPerSecond)
		for len(tl.steps) > 0 && !tl.steps[0].at.After(sim) {
			st := tl.steps[0]
			tl.steps = tl.steps[1:]
			if st.arrive {
				tl.arrived = true
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
	if err := s.publish(tl, s.estimate(estID, tl)); err != nil && !errors.Is(err, ErrNotQueued) {
		tl.shift(-delay) // nothing was said, so nothing changed
		return nil, err
	}
	alert.AlertID, alert.CauseEventIDs = s.nextID("AL"), []string{estID}
	if err := s.publish(tl, alert); err != nil && !errors.Is(err, ErrNotQueued) {
		// The new estimate is out, so the delay stands; only the alert is
		// missing. Retrying would delay the voyage again.
		return nil, fmt.Errorf("operator: the delay is applied and estimate %s published, but the alert was not: %w", estID, err)
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
