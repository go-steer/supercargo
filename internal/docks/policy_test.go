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

package docks

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/backend"
)

var (
	siteNow  = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	grantEnd = siteNow.Add(7 * 24 * time.Hour)
	truckSR  = TruckKey("serra.example", "SR-BKG-0913")
)

// guarulhos is a DC with an evening window, before any appointment.
func guarulhos() *Mock {
	return &Mock{Source: "Agendamento (simulated)", Site: "Guarulhos DC", Docks: []string{"Dock 1", "Dock 2"}, Zone: saoPaulo,
		Hours: []int{6, 10, 14, 18, 22}, Now: func() time.Time { return siteNow }}
}

func at(day, hour, minute int) time.Time {
	return time.Date(2026, 10, day, hour, minute, 0, 0, saoPaulo)
}

// TestPolicy: a truck the DC is granted gets the first slot it reaches in
// time to unload; the same arrival again leaves it; a later one moves it
// to the first slot that fits (the 22:00 window); an earlier one brings
// it back. Each says who set the policy.
func TestPolicy(t *testing.T) {
	ctx := context.Background()
	m := guarulhos()
	p := &Policy{Docks: m, By: "Receiving manager", Now: func() time.Time { return siteNow }}
	eta := at(25, 17, 0)
	o, err := p.Reserve(ctx, truckSR, "vv.example", grantEnd, eta)
	if err != nil || o.Action != Booked || o.Appointment.Policy != "standing dock policy, set by Receiving manager" ||
		o.Appointment.End.Before(eta.Add(time.Hour)) || o.Appointment.ShipperRef != truckSR {
		t.Fatalf("booked: %+v %v", o, err)
	}
	first := o.Appointment
	// Reserving again (a renewed grant) never moves it.
	if o, err := p.Reserve(ctx, truckSR, "vv.example", grantEnd, at(26, 22, 30)); err != nil || o.Action != Kept || !o.Appointment.Start.Equal(first.Start) {
		t.Errorf("reserved again: %+v %v", o, err)
	}
	late := at(26, 22, 30) // staged until 21:00, delivering 22:30
	for i := range 3 {
		o, err = p.Follow(ctx, truckSR, "vv.example", grantEnd, late)
		want := Moved
		if i > 0 {
			want = Kept // the same arrival: never moved twice
		}
		if err != nil || o.Action != want || !o.Appointment.Start.Equal(at(26, 22, 0)) || o.Appointment.Policy == "" {
			t.Fatalf("late, %d: %+v %v", i, o, err)
		}
	}
	if !o.Appointment.End.After(late) {
		t.Errorf("the slot ends before the truck: %+v", o.Appointment)
	}
	// Back on time: back to the first slot that fits.
	if o, err := p.Follow(ctx, truckSR, "vv.example", grantEnd, eta); err != nil || o.Action != Moved || !o.Appointment.Start.Equal(first.Start) || !o.FromStart.Equal(at(26, 22, 0)) {
		t.Errorf("on time again: %+v %v", o, err)
	}
}

// TestPolicyOwnSlot: with the slots before its own taken, a truck keeps
// the slot it holds: its own counts free, so it never moves later.
func TestPolicyOwnSlot(t *testing.T) {
	ctx := context.Background()
	m := guarulhos()
	p := &Policy{Docks: m, By: "x", Now: func() time.Time { return siteNow }}
	o, err := p.Reserve(ctx, truckSR, "vv.example", grantEnd, at(25, 21, 0))
	if err != nil || !o.Appointment.Start.Equal(at(25, 18, 0)) && !o.Appointment.Start.Equal(at(25, 22, 0)) {
		t.Fatalf("booked %+v %v", o, err)
	}
	held := o.Appointment
	// Others take every other slot that day.
	free, _ := m.Slots(ctx, at(25, 0, 0), 1)
	for i, s := range free {
		if _, err := m.Book(ctx, "other-"+string(rune('a'+i)), "", s); err != nil {
			t.Fatal(err)
		}
	}
	// A little earlier, still inside the held slot's reach: kept.
	if o, err := p.Follow(ctx, truckSR, "vv.example", grantEnd, at(25, 20, 30)); err != nil || o.Action != Kept || !o.Appointment.Start.Equal(held.Start) {
		t.Errorf("earlier: %+v %v", o, err)
	}
}

// TestPolicyPerson: a slot a person chose stands while the truck makes
// it; one it misses is the policy's to move.
func TestPolicyPerson(t *testing.T) {
	ctx := context.Background()
	m := guarulhos()
	p := &Policy{Docks: m, By: "x", Now: func() time.Time { return siteNow }}
	o, _ := p.Reserve(ctx, truckSR, "vv.example", grantEnd, at(25, 17, 0))
	free, _ := m.SlotsFor(ctx, at(26, 0, 0), 1, "")
	var night backend.DockSlot
	for _, s := range free {
		if s.Start.Hour() == 22 {
			night = s
		}
	}
	moved, err := m.Move(ctx, o.Appointment.ID, o.Appointment.Start, night) // a person: the 26th, 22:00
	if err != nil || moved.Policy != "" {
		t.Fatalf("a person's move: %+v %v", moved, err)
	}
	if o, err := p.Follow(ctx, truckSR, "vv.example", grantEnd, at(25, 19, 0)); err != nil || o.Action != Kept || !o.Appointment.Start.Equal(night.Start) {
		t.Errorf("the person's slot, still made: %+v %v", o, err)
	}
	if o, err := p.Follow(ctx, truckSR, "vv.example", grantEnd, at(27, 3, 30)); err != nil || o.Action != Moved || o.Appointment.Policy == "" {
		t.Errorf("missed: %+v %v", o, err)
	}
}

// TestPolicyBounds: a site keeps at most so many trucks, and one
// company's grants at most so many; a grant's end releases its slot.
func TestPolicyBounds(t *testing.T) {
	ctx := context.Background()
	m := guarulhos()
	now := siteNow
	p := &Policy{Docks: m, By: "x", MaxTrucks: 3, MaxPerIssuer: 2, Now: func() time.Time { return now }}
	eta := at(25, 17, 0)
	for i, issuer := range []string{"a.example", "a.example", "b.example"} {
		if _, err := p.Reserve(ctx, TruckKey("c.example", string(rune('1'+i))), issuer, now.Add(time.Hour), eta); err != nil {
			t.Fatalf("%d: %v", i, err)
		}
	}
	if _, err := p.Reserve(ctx, TruckKey("c.example", "4"), "c.example", grantEnd, eta); !errors.Is(err, ErrTooMany) {
		t.Errorf("a fourth truck: %v", err)
	}
	p.MaxTrucks = 10
	if _, err := p.Follow(ctx, TruckKey("c.example", "5"), "a.example", grantEnd, eta); !errors.Is(err, ErrTooMany) {
		t.Errorf("a's third: %v", err)
	}
	now = now.Add(2 * time.Hour) // the grants end
	if _, err := m.Appointment(ctx, TruckKey("c.example", "1")); err != nil {
		t.Fatalf("before pruning: %v", err)
	}
	if _, err := p.Reserve(ctx, TruckKey("c.example", "6"), "a.example", grantEnd, eta); err != nil {
		t.Errorf("after the grants ended: %v", err)
	}
	if _, err := m.Appointment(ctx, TruckKey("c.example", "1")); !errors.Is(err, ErrNoAppointment) {
		t.Errorf("an ended grant's slot: %v", err)
	}
}

// TestPolicyRefuses: no slot that fits, a slot taken meanwhile, and a
// schedule that fails, each fail; nothing is booked.
func TestPolicyRefuses(t *testing.T) {
	ctx := context.Background()
	m := guarulhos()
	eta := at(25, 17, 0)
	// A slot must start within a minute of the arrival and run 5h after
	// it: none does.
	tight := &Policy{Docks: m, By: "x", Unload: 5 * time.Hour, Ahead: time.Minute, Now: func() time.Time { return siteNow }}
	if o, err := tight.Reserve(ctx, "R", "i", grantEnd, eta); !errors.Is(err, ErrNoSlot) {
		t.Errorf("nothing within reach: %+v %v", o, err)
	}
	if _, err := m.Appointment(ctx, "R"); !errors.Is(err, ErrNoAppointment) {
		t.Errorf("booked anyway: %v", err)
	}
	free, _ := m.Slots(ctx, eta, 1)
	if _, err := m.Book(ctx, "A", "", free[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Book(ctx, "B", "", free[0]); !errors.Is(err, ErrSlotTaken) {
		t.Errorf("a taken slot: %v", err)
	}
	if _, err := m.Book(ctx, "B", "", backend.DockSlot{Dock: free[1].Dock, Start: free[1].Start, End: free[1].End.Add(time.Hour)}); !errors.Is(err, ErrSlotTaken) {
		t.Errorf("a slot with the wrong end: %v", err)
	}
	if _, err := m.MoveFor(ctx, "AG-404", eta, free[1], "x"); !errors.Is(err, ErrNoAppointment) {
		t.Errorf("no such appointment: %v", err)
	}
	if err := m.Release(ctx, "AG-404"); !errors.Is(err, ErrNoAppointment) {
		t.Errorf("releasing nothing: %v", err)
	}
	if _, err := m.SlotsFor(ctx, eta, 0, ""); err == nil {
		t.Error("no days")
	}
	for name, f := range map[string]func(*Policy) error{
		"reserve": func(p *Policy) error { _, err := p.Reserve(ctx, "C", "i", grantEnd, eta); return err },
		"follow":  func(p *Policy) error { _, err := p.Follow(ctx, "C", "i", grantEnd, eta); return err },
	} {
		if err := f(&Policy{Docks: failing{m}, By: "x"}); err == nil {
			t.Errorf("%s: a failing schedule", name)
		}
	}
	// Follow on a truck it can't fit anywhere is refused, and stays.
	p := &Policy{Docks: m, By: "x", Now: func() time.Time { return siteNow }}
	o, err := p.Reserve(ctx, "D", "i", grantEnd, eta)
	if err != nil {
		t.Fatal(err)
	}
	p.Unload, p.Ahead = 5*time.Hour, time.Minute
	if _, err := p.Follow(ctx, "D", "i", grantEnd, at(25, 23, 0)); !errors.Is(err, ErrNoSlot) {
		t.Errorf("no slot to move to: %v", err)
	}
	if a, _ := m.Appointment(ctx, "D"); !a.Start.Equal(o.Appointment.Start) {
		t.Errorf("moved anyway: %+v", a)
	}
}

// failing is a schedule whose reads fail.
type failing struct{ *Mock }

func (failing) Appointment(context.Context, string) (*backend.Appointment, error) {
	return nil, errors.New("down")
}
