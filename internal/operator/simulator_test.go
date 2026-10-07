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
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/intents"
)

// published records what the simulator publishes.
type published struct {
	mu     sync.Mutex
	bodies []intents.Body
	fail   error
	// failAlerts fails alerts only.
	failAlerts error
}

func (p *published) Publish(_ store.Booking, body intents.Body) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail != nil {
		return p.fail
	}
	if _, ok := body.(*intents.TnTAlertBody); ok && p.failAlerts != nil {
		return p.failAlerts
	}
	if err := body.Validate(); err != nil {
		return err
	}
	p.bodies = append(p.bodies, body)
	return nil
}

func (p *published) list() []intents.Body {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]intents.Body(nil), p.bodies...)
}

// summary renders a body as "CODE CLASSIFIER [container]" or "alert KIND".
func summary(b intents.Body) string {
	switch b := b.(type) {
	case *intents.TnTEventBody:
		s := b.EventTypeCode + " " + b.EventClassifierCode
		if b.EquipmentReference != "" {
			s += " " + b.EquipmentReference
		}
		return s
	case *intents.TnTAlertBody:
		return "alert " + b.Kind
	}
	return "?"
}

// simulated is a simulator with a fake clock, and a booking service that
// starts a voyage for each booking. The voyage departs 2026-10-07 18:00
// and arrives 134h later, 2026-10-13 08:00.
type simulated struct {
	sim     *Simulator
	pub     *published
	svc     *BookingService
	now     time.Time
	errs    []string
	started []error
}

func newSimulated(t *testing.T) *simulated {
	t.Helper()
	s := &simulated{pub: &published{}, now: time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)}
	s.sim = &Simulator{Publisher: s.pub, HoursPerSecond: 1, StartDelay: time.Second, Now: func() time.Time { return s.now },
		OnError: func(ref string, err error) { s.errs = append(s.errs, ref+": "+err.Error()) }}
	s.svc = service()
	s.svc.OnBooked = func(b store.Booking) { s.started = append(s.started, s.sim.Start(b)) }
	return s
}

// book books n containers with a subscription and returns the reference.
func (s *simulated) book(t *testing.T, n int) string {
	t.Helper()
	b := body()
	b.Equipment = []intents.Equipment{{ISOType: "45G1", Label: "40HC", Count: n}}
	b.Subscription = &intents.Subscription{Endpoint: "https://cymbal-shoes.example/ulp", Intents: []string{intents.TnTEvent, intents.TnTAlert}}
	reply, err := s.svc.Handle(context.Background(), request("cymbal-shoes.example", b))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.started[len(s.started)-1]; err != nil {
		t.Fatalf("voyage not started: %v", err)
	}
	return reply.BookingRef
}

// advance moves the real clock by d and lets the simulator catch up.
func (s *simulated) advance(d time.Duration) {
	s.now = s.now.Add(d)
	s.sim.Advance()
}

func TestVoyage(t *testing.T) {
	s := newSimulated(t)
	ref := s.book(t, 2)
	s.advance(999 * time.Millisecond)
	if n := len(s.pub.list()); n != 0 {
		t.Fatalf("%d events before the clock started", n)
	}
	// 1s of start delay, then 27h lead-in + 134h transit + 30h to gate-out.
	for range 200 {
		s.advance(time.Second)
	}
	var got []string
	ids := map[string]bool{}
	var last time.Time
	for _, b := range s.pub.list() {
		got = append(got, summary(b))
		e := b.(*intents.TnTEventBody)
		if ids[e.EventID] {
			t.Errorf("event ID %s used twice", e.EventID)
		}
		ids[e.EventID] = true
		if e.EventClassifierCode == intents.ClassifierActual {
			at, _ := time.Parse(time.RFC3339, e.EventDateTime)
			if at.Before(last) {
				t.Errorf("%s at %s, before the previous actual event", summary(b), e.EventDateTime)
			}
			last = at
		}
	}
	want := []string{
		"GTIN ACT OCBU0000004", "GTIN ACT OCBU0000010", "LOAD ACT OCBU0000004", "LOAD ACT OCBU0000010",
		"DEPA ACT", "ARRI EST", "ARRI ACT",
		"DISC ACT OCBU0000004", "DISC ACT OCBU0000010", "GTOT ACT OCBU0000004", "GTOT ACT OCBU0000010",
	}
	if strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Errorf("voyage:\n got %v\nwant %v", got, want)
	}
	evs := s.pub.list()
	if dep := evs[4].(*intents.TnTEventBody); dep.EventDateTime != "2026-10-07T18:00:00Z" || dep.VesselName != "Ocean Blue Aurora" || dep.UNLocationCode != "NLRTM" || dep.Description != "Ocean Blue Aurora 42W departed Port of Rotterdam" {
		t.Errorf("departure %+v", dep)
	}
	if arr := evs[6].(*intents.TnTEventBody); arr.EventDateTime != "2026-10-13T08:00:00Z" || arr.UNLocationCode != "USEWR" || arr.FacilityName != "Port of Newark" {
		t.Errorf("arrival %+v", arr)
	}
	// A finished voyage is forgotten.
	if _, err := s.sim.Disrupt(ref, time.Hour, "Late."); !errors.Is(err, ErrUnknownBooking) {
		t.Errorf("Disrupt after the voyage = %v, want ErrUnknownBooking", err)
	}
	if len(s.errs) != 0 {
		t.Errorf("publish errors: %v", s.errs)
	}
}

func TestClockSpeed(t *testing.T) {
	s := newSimulated(t)
	s.sim.HoursPerSecond = 10
	s.book(t, 1)
	// Gate-in is 1h of simulated time after the clock starts: 0.1s at 10h/s.
	s.advance(time.Second + 90*time.Millisecond)
	if n := len(s.pub.list()); n != 0 {
		t.Fatalf("%d events before gate-in", n)
	}
	s.advance(20 * time.Millisecond)
	if got := s.pub.list(); len(got) != 1 || summary(got[0]) != "GTIN ACT OCBU0000004" {
		t.Errorf("after gate-in: %v", got)
	}
}

func TestDisrupt(t *testing.T) {
	s := newSimulated(t)
	ref := s.book(t, 1)
	// Past the departure (27h after the clock starts) and its estimate.
	s.advance(time.Second + 29*time.Second)
	before := len(s.pub.list())
	if summary(s.pub.list()[before-1]) != "ARRI EST" {
		t.Fatalf("last before the disruption: %v", summary(s.pub.list()[before-1]))
	}

	alert, err := s.sim.Disrupt(ref, 72*time.Hour, "Engine repair, unscheduled call at Halifax.")
	if err != nil {
		t.Fatal(err)
	}
	got := s.pub.list()[before:]
	if len(got) != 2 {
		t.Fatalf("Disrupt published %d bodies, want an estimate and an alert", len(got))
	}
	est := got[0].(*intents.TnTEventBody)
	if est.EventTypeCode != "ARRI" || est.EventClassifierCode != intents.ClassifierEstimated || est.EventDateTime != "2026-10-16T08:00:00Z" {
		t.Errorf("new estimate %+v", est)
	}
	if got[1] != intents.Body(alert) || alert.Kind != intents.AlertDelay || alert.Severity != intents.SeverityHigh ||
		alert.PreviousETA != "2026-10-13T08:00:00Z" || alert.NewETA != "2026-10-16T08:00:00Z" ||
		len(alert.CauseEventIDs) != 1 || alert.CauseEventIDs[0] != est.EventID {
		t.Errorf("alert %+v", alert)
	}

	// The arrival moved: nothing at the old time, ARRI at the new one.
	s.advance(134*time.Second + 6*time.Second) // the old discharge time
	if last := s.pub.list(); summary(last[len(last)-1]) != "alert delay" {
		t.Errorf("an event fired at the old arrival time: %s", summary(last[len(last)-1]))
	}
	s.advance(72 * time.Second)
	last := s.pub.list()
	arr := last[len(last)-2].(*intents.TnTEventBody)
	if arr.EventTypeCode != "ARRI" || arr.EventClassifierCode != intents.ClassifierActual || arr.EventDateTime != "2026-10-16T08:00:00Z" {
		t.Errorf("arrival %+v", arr)
	}
	if _, err := s.sim.Disrupt(ref, time.Hour, "Late."); !errors.Is(err, ErrArrived) {
		t.Errorf("Disrupt after arrival = %v, want ErrArrived", err)
	}
	// Discharge and gate-out moved with the arrival, in when they fire and
	// in the times they report.
	s.advance(30 * time.Second)
	last = s.pub.list()
	disc, gtot := last[len(last)-2].(*intents.TnTEventBody), last[len(last)-1].(*intents.TnTEventBody)
	if disc.EventTypeCode != "DISC" || disc.EventDateTime != "2026-10-16T14:00:00Z" ||
		gtot.EventTypeCode != "GTOT" || gtot.EventDateTime != "2026-10-17T14:00:00Z" {
		t.Errorf("after the delay: %s at %s, %s at %s", disc.EventTypeCode, disc.EventDateTime, gtot.EventTypeCode, gtot.EventDateTime)
	}
}

// TestDisruptTotalCapped: delays add up to at most 30 days per voyage.
func TestDisruptTotalCapped(t *testing.T) {
	s := newSimulated(t)
	ref := s.book(t, 1)
	s.advance(2 * time.Second)
	if _, err := s.sim.Disrupt(ref, 20*24*time.Hour, "Late."); err != nil {
		t.Fatal(err)
	}
	if _, err := s.sim.Disrupt(ref, 11*24*time.Hour, "Later."); err == nil {
		t.Error("a voyage was delayed over 30 days in all")
	}
	if _, err := s.sim.Disrupt(ref, 10*24*time.Hour, "Later."); err != nil {
		t.Errorf("up to 30 days in all: %v", err)
	}
}

// TestShortVoyageHasNoEstimate: a transit too short for an estimate
// after departure gets none, so nothing is estimated after arriving.
func TestShortVoyageHasNoEstimate(t *testing.T) {
	s := newSimulated(t)
	s.svc.Voyage.Transit = time.Hour
	s.book(t, 1)
	for range 100 {
		s.advance(time.Second)
	}
	for _, b := range s.pub.list() {
		if summary(b) == "ARRI EST" {
			t.Error("a one-hour voyage published an arrival estimate")
		}
	}
}

func TestDisruptRejects(t *testing.T) {
	s := newSimulated(t)
	ref := s.book(t, 1)
	s.advance(2 * time.Second)
	n := len(s.pub.list())
	for name, tc := range map[string]struct {
		ref    string
		delay  time.Duration
		reason string
	}{
		"unknown booking": {"OB-BKG-9999", time.Hour, "Late."},
		"zero delay":      {ref, 0, "Late."},
		"negative delay":  {ref, -time.Hour, "Late."},
		"over 30 days":    {ref, 31 * 24 * time.Hour, "Late."},
		"empty reason":    {ref, time.Hour, ""},
		"two lines":       {ref, time.Hour, "Late.\nVery late."},
		"bidi override":   {ref, time.Hour, "Late \u202e."},
		"over 280 runes":  {ref, time.Hour, strings.Repeat("x", 281)},
	} {
		if _, err := s.sim.Disrupt(tc.ref, tc.delay, tc.reason); err == nil {
			t.Errorf("%s: Disrupt succeeded", name)
		}
	}
	if got := len(s.pub.list()); got != n {
		t.Errorf("refused disruptions published %d bodies", got-n)
	}
	// Nothing moved: the arrival is still on the 13th.
	for range 200 {
		s.advance(time.Second)
	}
	for _, b := range s.pub.list() {
		if e := b.(*intents.TnTEventBody); e.EventTypeCode == "ARRI" && e.EventDateTime != "2026-10-13T08:00:00Z" {
			t.Errorf("a refused disruption moved the arrival: %+v", e)
		}
	}
}

func TestSeverity(t *testing.T) {
	for d, want := range map[time.Duration]string{
		time.Hour: intents.SeverityLow, 12 * time.Hour: intents.SeverityMedium, 48 * time.Hour: intents.SeverityHigh,
	} {
		if got := severity(d); got != want {
			t.Errorf("severity(%v) = %s, want %s", d, got, want)
		}
	}
}

func TestStartRejects(t *testing.T) {
	s := newSimulated(t)
	s.sim.MaxTimelines = 1
	if s.sim.Active() != 0 {
		t.Error("voyages before any booking")
	}
	ref := s.book(t, 1)
	if s.sim.Active() != 1 {
		t.Errorf("Active = %d, want 1", s.sim.Active())
	}
	b, _ := s.svc.Bookings.Get("ocean-blue.example", ref)
	if err := s.sim.Start(b); err == nil {
		t.Error("a second voyage for the same booking started")
	}
	// Past the cap: booked, but no voyage.
	if _, err := s.svc.Handle(context.Background(), request("cymbal-shoes.example", body())); err != nil {
		t.Fatal(err)
	}
	if err := s.started[len(s.started)-1]; !errors.Is(err, ErrTooManyVoyages) {
		t.Errorf("Start past MaxTimelines = %v, want ErrTooManyVoyages", err)
	}
	rejected := b
	rejected.Ref = "OB-BKG-1"
	rejected.Confirmation.Status = intents.StatusRejected
	if err := s.sim.Start(rejected); err == nil {
		t.Error("a rejected booking got a voyage")
	}
	bad := b
	bad.Ref = "OB-BKG-2"
	bad.Confirmation.PlannedArrival = "soon"
	if err := s.sim.Start(bad); err == nil {
		t.Error("a booking without planned times got a voyage")
	}
}

func TestPublishErrorsReported(t *testing.T) {
	s := newSimulated(t)
	ref := s.book(t, 1)
	s.pub.fail = errors.New("queue full")
	s.advance(3 * time.Second)
	if len(s.errs) == 0 || !strings.HasPrefix(s.errs[0], ref+": ") {
		t.Errorf("errors %v", s.errs)
	}
	if _, err := s.sim.Disrupt(ref, time.Hour, "Late."); err == nil {
		t.Error("Disrupt reported success when its estimate wasn't published")
	}
	// Nothing was said, so nothing moved: the next delay starts from the
	// planned arrival.
	s.pub.fail = nil
	a, err := s.sim.Disrupt(ref, time.Hour, "Late.")
	if err != nil || a.PreviousETA != "2026-10-13T08:00:00Z" || a.NewETA != "2026-10-13T09:00:00Z" {
		t.Errorf("after a failed disruption: %+v, %v", a, err)
	}
	// Recorded but not queued is still said: the delay stands.
	s.pub.fail = fmt.Errorf("x: %w", ErrNotQueued)
	if _, err := s.sim.Disrupt(ref, time.Hour, "Later."); err != nil {
		t.Errorf("a disruption recorded but not queued: %v", err)
	}
	// The estimate out but the alert not: an error, and the delay stands.
	s.pub.fail, s.pub.failAlerts = nil, errors.New("publisher closed")
	if _, err := s.sim.Disrupt(ref, time.Hour, "Later still."); err == nil || !strings.Contains(err.Error(), "delay is applied") {
		t.Errorf("alert not published: %v", err)
	}
	s.pub.failAlerts = nil
	a, err = s.sim.Disrupt(ref, time.Hour, "Late again.")
	if err != nil || a.PreviousETA != "2026-10-13T11:00:00Z" {
		t.Errorf("the delays didn't add up: %+v, %v", a, err)
	}
}

// TestRunAndClose: Run advances on its own, and Close stops it.
func TestRunAndClose(t *testing.T) {
	pub := &published{}
	sim := &Simulator{Publisher: pub, HoursPerSecond: 3600, Tick: time.Millisecond, StartDelay: time.Millisecond}
	svc := service()
	svc.OnBooked = func(b store.Booking) {
		if err := sim.Start(b); err != nil {
			t.Error(err)
		}
	}
	go sim.Run()
	b := body()
	b.Equipment = []intents.Equipment{{ISOType: "45G1", Label: "40HC", Count: 1}}
	if _, err := svc.Handle(context.Background(), request("cymbal-shoes.example", b)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(pub.list()) == 7 })
	sim.Close()
	sim.Close()  // idempotent
	go sim.Run() // after Close: returns at once
	// Close without Run returns too.
	(&Simulator{}).Close()
	if got := fmt.Sprint(len(pub.list())); got != "7" {
		t.Errorf("%s events", got)
	}
}

// TestLongNamesStillPublish: a 128-character place or vessel name (the
// booking's limits) still makes valid events: the facility and the
// description are cut to theirs.
func TestLongNamesStillPublish(t *testing.T) {
	long := strings.Repeat("x", 128)
	tl := &timeline{booking: store.Booking{Ref: "OB-1",
		Request:      intents.BookingRequestBody{Destination: intents.Location{UNLocode: "USEWR", Name: long}},
		Confirmation: intents.BookingConfirmationBody{Vessel: &intents.Vessel{Name: long, Voyage: "42W"}}},
		arrival: time.Date(2026, 10, 13, 8, 0, 0, 0, time.UTC)}
	s := &Simulator{}
	ev := s.estimate("EV-1", tl, strings.Repeat("r", 280)).(*intents.TnTEventBody)
	if err := ev.Validate(); err != nil {
		t.Errorf("estimate with long names: %v", err)
	}
	if f := facility(intents.Location{Name: long}); len([]rune(f)) != 128 || !strings.HasSuffix(f, "…") {
		t.Errorf("facility %d characters", len([]rune(f)))
	}
	if d := describe("%s %s departed %s", long, "42W", facility(intents.Location{Name: long})); len([]rune(d)) > 280 {
		t.Errorf("description %d characters", len([]rune(d)))
	}
}
