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
	"fmt"
	"sync"
	"time"

	"github.com/go-steer/supercargo/internal/backend"
)

// Schedule is a dock schedule a standing policy may act on.
type Schedule interface {
	backend.DockAppointments
	backend.DockBooker
}

// Policy is a receiving site's standing dock policy (docs/DESIGN.md,
// "Standing dock policy"): set ahead of time by a person (By), it keeps
// an appointment for each truck the site follows by delegation (by
// TruckKey), in the first slot the truck reaches in time to unload. A
// truck it's granted gets one (Reserve); a new arrival from the carrier
// moves it to the first slot that fits, its own counted free, so the same
// arrival never moves it twice (Follow). A person's move stands while the
// truck still makes it. Its appointments are bounded, and released when
// the grant ends.
type Policy struct {
	Docks Schedule
	By    string
	// Unload is how long a slot must run after the arrival (default 1h).
	Unload time.Duration
	// Ahead bounds how far past the arrival a slot may start (default
	// 36h): beyond it the policy leaves the truck to a person.
	Ahead time.Duration
	// MaxTrucks bounds the trucks it keeps appointments for (default
	// 50); MaxPerIssuer, those one company's grants bring (default 20).
	MaxTrucks, MaxPerIssuer int
	Now                     func() time.Time

	mu     sync.Mutex
	trucks map[string]*truck // by TruckKey
}

// truck is an appointment the policy keeps: whose grant brought it, until
// when, and the arrival it last acted on.
type truck struct {
	id, issuer string
	until, eta time.Time
}

// Outcome is what the policy did for a truck: "booked", "moved" or
// "kept", the appointment as it stands, and where it was before a move.
type Outcome struct {
	Action      string               `json:"action"`
	Appointment *backend.Appointment `json:"appointment"`
	FromDock    string               `json:"fromDock,omitempty"`
	FromStart   time.Time            `json:"fromStart,omitzero"`
}

// Policy outcomes.
const (
	Booked = "booked"
	Moved  = "moved"
	Kept   = "kept"
)

var (
	// ErrNoSlot is a truck no free slot fits within the policy's reach.
	ErrNoSlot = errors.New("docks: no free slot fits the arrival")
	// ErrTooMany is a truck beyond the policy's bounds.
	ErrTooMany = errors.New("docks: the policy keeps no more trucks")
)

// Note is what an appointment the policy touched says.
func (p *Policy) Note() string { return "standing dock policy, set by " + p.By }

func (p *Policy) unload() time.Duration {
	if p.Unload > 0 {
		return p.Unload
	}
	return time.Hour
}

func (p *Policy) ahead() time.Duration {
	if p.Ahead > 0 {
		return p.Ahead
	}
	return 36 * time.Hour
}

func (p *Policy) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// fits: the truck arriving at eta unloads inside the slot, and the slot
// doesn't start beyond the policy's reach.
func (p *Policy) fits(start, end, eta time.Time) bool {
	return !eta.Add(p.unload()).After(end) && start.Before(eta.Add(p.ahead()))
}

// Reserve is a truck the site was just granted (key, by issuer's grant
// until until), planned to arrive at eta: an appointment if it has none.
// It never moves one: a newer arrival may already have.
func (p *Policy) Reserve(ctx context.Context, key, issuer string, until, eta time.Time) (*Outcome, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prune(ctx)
	a, err := p.Docks.Appointment(ctx, key)
	switch {
	case err == nil:
		p.track(key, issuer, a.ID, until, time.Time{})
		return &Outcome{Action: Kept, Appointment: a}, nil
	case !errors.Is(err, ErrNoAppointment):
		return nil, err
	}
	return p.book(ctx, key, issuer, until, eta)
}

// Follow is a truck's new arrival, from its carrier's verified alert:
// its appointment moves to the first slot that fits, if that isn't where
// it is (one a person chose stays while the truck makes it).
func (p *Policy) Follow(ctx context.Context, key, issuer string, until, eta time.Time) (*Outcome, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prune(ctx)
	a, err := p.Docks.Appointment(ctx, key)
	switch {
	case errors.Is(err, ErrNoAppointment):
		return p.book(ctx, key, issuer, until, eta)
	case err != nil:
		return nil, err
	}
	t := p.track(key, issuer, a.ID, until, time.Time{})
	if t.eta.Equal(eta) {
		return &Outcome{Action: Kept, Appointment: a}, nil // acted on already
	}
	if a.Policy == "" && !eta.Add(p.unload()).After(a.End) {
		t.eta = eta // a person's choice, and the truck still makes it
		return &Outcome{Action: Kept, Appointment: a}, nil
	}
	s, err := p.slot(ctx, eta, a.ID)
	if err != nil {
		return nil, err
	}
	if s.Dock == a.Dock && s.Start.Equal(a.Start) {
		t.eta = eta
		return &Outcome{Action: Kept, Appointment: a}, nil
	}
	m, err := p.Docks.MoveFor(ctx, a.ID, a.Start, *s, p.Note())
	if err != nil {
		return nil, err
	}
	t.eta = eta
	return &Outcome{Action: Moved, Appointment: m, FromDock: a.Dock, FromStart: a.Start}, nil
}

// book is a new appointment for key, within the bounds; p.mu is held.
func (p *Policy) book(ctx context.Context, key, issuer string, until, eta time.Time) (*Outcome, error) {
	maxTrucks, perIssuer := p.MaxTrucks, p.MaxPerIssuer
	if maxTrucks <= 0 {
		maxTrucks = 50
	}
	if perIssuer <= 0 {
		perIssuer = 20
	}
	n := 0
	for _, t := range p.trucks {
		if t.issuer == issuer {
			n++
		}
	}
	if len(p.trucks) >= maxTrucks || n >= perIssuer {
		return nil, ErrTooMany
	}
	s, err := p.slot(ctx, eta, "")
	if err != nil {
		return nil, err
	}
	b, err := p.Docks.Book(ctx, key, p.Note(), *s)
	if err != nil {
		return nil, err
	}
	p.track(key, issuer, b.ID, until, eta)
	return &Outcome{Action: Booked, Appointment: b}, nil
}

// track records the truck (a later grant's issuer and end win); p.mu is
// held.
func (p *Policy) track(key, issuer, id string, until, eta time.Time) *truck {
	if p.trucks == nil {
		p.trucks = map[string]*truck{}
	}
	t, ok := p.trucks[key]
	if !ok {
		t = &truck{eta: eta}
		p.trucks[key] = t
	}
	t.id, t.issuer, t.until = id, issuer, until
	return t
}

// prune releases the appointments whose grants have ended; p.mu is held.
func (p *Policy) prune(ctx context.Context) {
	now := p.now()
	for key, t := range p.trucks {
		if !now.Before(t.until) {
			_ = p.Docks.Release(ctx, t.id) // gone already is as good
			delete(p.trucks, key)
		}
	}
}

// slot is the first free slot that fits eta, appointment except's own
// counted free.
func (p *Policy) slot(ctx context.Context, eta time.Time, except string) (*backend.DockSlot, error) {
	// From the day before (a slot from 22:00 runs into eta's day), for
	// as many days as the reach needs.
	days := int(p.ahead()/(24*time.Hour)) + 3
	free, err := p.Docks.SlotsFor(ctx, eta.Add(-24*time.Hour), min(days, MaxDays), except)
	if err != nil {
		return nil, err
	}
	for _, s := range free { // in order of start
		if p.fits(s.Start, s.End, eta) {
			return &s, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrNoSlot, eta.UTC().Format(time.RFC3339))
}
