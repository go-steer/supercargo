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
	"encoding/json"
	"errors"
	"slices"
	"strings"
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

// Booking is what a party records about one booking: an ocean booking
// (Request and Confirmation), or a truck leg (Road, with Request and
// Confirmation left empty).
type Booking struct {
	Ref          string
	Shipper      string // domain of the party that requested it
	Operator     string // domain of the party that confirmed it
	Request      intents.BookingRequestBody
	Confirmation intents.BookingConfirmationBody
	Road         *RoadBooking
	CreatedAt    time.Time
}

// RoadBooking is a truck leg's request, as amended, and the carrier's
// latest confirmation.
type RoadBooking struct {
	Request      intents.RoadBookingRequestBody
	Confirmation intents.RoadBookingConfirmationBody
}

// Subscription is the booking's subscription, or nil.
func (b *Booking) Subscription() *intents.Subscription {
	if b.Road != nil {
		return b.Road.Request.Subscription
	}
	return b.Request.Subscription
}

// SubscriptionID is the subscription's ID from the confirmation.
func (b *Booking) SubscriptionID() string {
	if b.Road != nil {
		return b.Road.Confirmation.SubscriptionID
	}
	return b.Confirmation.SubscriptionID
}

// EquipmentRefs are the booking's containers: the eBL's, or those a truck
// leg collects.
func (b *Booking) EquipmentRefs() []string {
	if b.Road != nil {
		return slices.Clone(b.Road.Request.EquipmentReferences)
	}
	return b.Confirmation.EquipmentRefs()
}

// Places are where the booking starts and ends (UN/LOCODEs): the ports of
// loading and discharge, or a truck leg's pickup and delivery.
func (b *Booking) Places() (from, to string) {
	if b.Road != nil {
		return b.Road.Request.Pickup.UNLocationCode, b.Road.Request.Delivery.UNLocationCode
	}
	return b.Request.PortOfLoading, b.Request.PortOfDischarge
}

// ShipperRef is the shipper's own reference for the booking.
func (b *Booking) ShipperRef() string {
	if b.Road != nil {
		return b.Road.Request.ShipperRef
	}
	return b.Request.ShipperRef
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

// Update replaces the booking operator holds under ref with what change
// makes of a copy of it, atomically. If change fails, nothing changes.
// The operator and reference can't change.
func (s *Bookings) Update(operator, ref string, change func(*Booking) error) (Booking, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := bookingKey{operator, ref}
	b, ok := s.m[k]
	if !ok {
		return Booking{}, ErrNotFound
	}
	b = clone(b)
	if err := change(&b); err != nil {
		return Booking{}, err
	}
	b.Operator, b.Ref = operator, ref
	s.m[k] = clone(b)
	return clone(b), nil
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

// List returns copies of all bookings, newest first (by creation, then
// reference).
func (s *Bookings) List() []Booking {
	s.mu.Lock()
	out := make([]Booking, 0, len(s.m))
	for _, b := range s.m {
		out = append(out, clone(b))
	}
	s.mu.Unlock()
	slices.SortFunc(out, func(a, b Booking) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.Operator+"/"+a.Ref, b.Operator+"/"+b.Ref)
	})
	return out
}

// Operators returns the operators that hold a booking under ref, sorted: a
// shipper names a booking by its reference, which two operators may share.
func (s *Bookings) Operators(ref string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.m {
		if k.ref == ref {
			out = append(out, k.operator)
		}
	}
	slices.Sort(out)
	return out
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
	if c.EBL != nil {
		e := *c.EBL
		e.Containers = slices.Clone(e.Containers)
		for i, ct := range e.Containers {
			if ct.TargetTemperatureCelsius != nil {
				t := *ct.TargetTemperatureCelsius
				e.Containers[i].TargetTemperatureCelsius = &t
			}
		}
		c.EBL = &e
	}
	c.Messages = slices.Clone(c.Messages)
	if b.Road != nil {
		// The road bodies nest deeply (parties, addresses, stops); a JSON
		// round trip copies all of it, and they're JSON by nature.
		// (Plain structs of strings, numbers and slices: they always
		// marshal and unmarshal.)
		raw, _ := json.Marshal(b.Road)
		r := &RoadBooking{}
		_ = json.Unmarshal(raw, r)
		b.Road = r
	}
	return b
}
