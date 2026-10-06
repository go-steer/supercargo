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

package store

import (
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/go-steer/supercargo/pkg/intents"
)

var (
	// ErrNotFound reports a booking the store doesn't hold.
	ErrNotFound = errors.New("store: booking not found")

	// ErrExists reports an attempt to store a booking under a key already
	// taken.
	ErrExists = errors.New("store: booking already exists")
)

// Booking is what a party records about one booking.
type Booking struct {
	Ref          string
	Shipper      string // domain of the party that requested it
	Operator     string // domain of the party that confirmed it
	Request      intents.BookingRequestBody
	Confirmation intents.BookingConfirmationBody
	CreatedAt    time.Time
}

// Bookings holds bookings by (operator, reference). A reference is only
// unique per operator: keying by reference alone would let one operator's
// confirmation overwrite another's booking. It is safe for concurrent use,
// and copies bookings deeply on the way in and out.
type Bookings struct {
	max int

	mu sync.Mutex
	m  map[bookingKey]Booking
}

// MaxBookings bounds a Bookings store. Anyone may request a booking
// (docs/DESIGN.md, A.6), so without a bound requests could grow the
// operator's memory without limit.
const MaxBookings = 100_000

type bookingKey struct{ operator, ref string }

// NewBookings returns an empty store holding at most MaxBookings.
func NewBookings() *Bookings { return NewBookingsCap(MaxBookings) }

// NewBookingsCap returns an empty store holding at most n bookings.
func NewBookingsCap(n int) *Bookings { return &Bookings{max: n, m: map[bookingKey]Booking{}} }

// Add stores b under (b.Operator, b.Ref). It never replaces a booking:
// ErrExists if the key is taken, ErrFull if the store is at capacity.
func (s *Bookings) Add(b Booking) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := bookingKey{b.Operator, b.Ref}
	if _, ok := s.m[k]; ok {
		return ErrExists
	}
	if len(s.m) >= s.max {
		return ErrFull
	}
	s.m[k] = clone(b)
	return nil
}

// Get returns the booking operator holds under ref.
func (s *Bookings) Get(operator, ref string) (Booking, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.m[bookingKey{operator, ref}]
	if !ok {
		return Booking{}, ErrNotFound
	}
	return clone(b), nil
}

// Len is the number of bookings held.
func (s *Bookings) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

// clone copies b deeply, so neither the caller nor the store can change the
// other's copy through a shared slice or pointer.
func clone(b Booking) Booking {
	r := &b.Request
	r.Equipment = slices.Clone(r.Equipment)
	if r.Consignee != nil {
		c := *r.Consignee
		r.Consignee = &c
	}
	if r.Subscription != nil {
		s := *r.Subscription
		s.Intents = slices.Clone(s.Intents)
		r.Subscription = &s
	}
	c := &b.Confirmation
	if c.Vessel != nil {
		v := *c.Vessel
		c.Vessel = &v
	}
	c.EquipmentRefs = slices.Clone(c.EquipmentRefs)
	c.Messages = slices.Clone(c.Messages)
	return b
}
