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
// (Request and Confirmation), a truck leg (Road), or a forwarding order
// (Order), with Request and Confirmation left empty for the last two.
type Booking struct {
	Ref          string
	Shipper      string // domain of the party that requested it
	Operator     string // domain of the party that confirmed it
	Request      intents.BookingRequestBody
	Confirmation intents.BookingConfirmationBody
	Road         *RoadBooking
	Order        *OrderBooking
	CreatedAt    time.Time
	// CancelledAt is when the booking was cancelled (its carrier answered
	// a cancellation so), zero while it stands.
	CancelledAt time.Time
}

// Cancelled reports whether the booking was cancelled.
func (b *Booking) Cancelled() bool { return !b.CancelledAt.IsZero() }

// OrderBooking is a forwarding order: the shipper's request, the ID of
// the message it came in (the forwarder's answers reply to it), and the
// forwarder's latest answer (pending, then confirmed or rejected).
type OrderBooking struct {
	Request      intents.ForwardingRequestBody
	MessageID    string
	Confirmation intents.ForwardingConfirmationBody
	// Legs are the legs the forwarder booked for the order, in journey
	// order (the forwarder's record; empty on the shipper's side).
	Legs []LegRef
	// Withdrawn are legs booked for the order and then cancelled with
	// their carriers, in the order they were cancelled (the forwarder's
	// record).
	Withdrawn []LegRef
	// Rates are freight rates a person set for this order's legs (by leg,
	// from 1), in place of the lane's (the forwarder's record, never sent).
	Rates map[int]float64 `json:",omitempty"`
}

// LegRef is one leg a forwarder booked for an order: with whom, under
// which reference (the leg's own booking, held as the leg's shipper), and
// what the carrier planned.
type LegRef struct {
	Mode       string   `json:"mode"`    // road or ocean
	Carrier    string   `json:"carrier"` // the carrier's domain
	Name       string   `json:"name"`    // the carrier's name, as the customer is told it
	Ref        string   `json:"ref"`     // the leg's booking reference with the carrier
	From       string   `json:"from"`    // UN/LOCODEs
	To         string   `json:"to"`
	Departure  string   `json:"departure,omitempty"` // planned, UTC
	Arrival    string   `json:"arrival,omitempty"`
	Vessel     string   `json:"vessel,omitempty"`     // an ocean leg's vessel and voyage
	Containers []string `json:"containers,omitempty"` // the containers it carries, once known
	// Unsigned marks a leg booked outside ULP (over EDI): its carrier
	// signs nothing, and CarrierRef and Documents are what it reported.
	Unsigned   bool     `json:"unsigned,omitempty"`
	CarrierRef string   `json:"carrierRef,omitempty"`
	Documents  []string `json:"documents,omitempty"`
}

// RoadBooking is a truck leg's request, as amended, and the carrier's
// latest confirmation.
type RoadBooking struct {
	Request      intents.RoadBookingRequestBody
	Confirmation intents.RoadBookingConfirmationBody
	// BackOffice is what the carrier's own systems behind its node did for
	// the leg (the carrier's record, for its people; never sent).
	BackOffice []BackOfficeStep `json:",omitempty"`
}

// BackOfficeStep is one thing a carrier's system did for a booking: its
// TMS issuing a fiscal document, its risk manager clearing the truck, a
// toll voucher issued.
type BackOfficeStep struct {
	System    string    `json:"system"` // tms, risk, tolls
	Source    string    `json:"source"` // who did it, e.g. "Trafegus (simulated)"
	Action    string    `json:"action"`
	Reference string    `json:"reference,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	At        time.Time `json:"at"`
}

// Subscription is the booking's subscription, or nil.
func (b *Booking) Subscription() *intents.Subscription {
	switch {
	case b.Road != nil:
		return b.Road.Request.Subscription
	case b.Order != nil:
		return b.Order.Request.Subscription
	}
	return b.Request.Subscription
}

// SubscriptionID is the subscription's ID from the confirmation.
func (b *Booking) SubscriptionID() string {
	switch {
	case b.Road != nil:
		return b.Road.Confirmation.SubscriptionID
	case b.Order != nil:
		return b.Order.Confirmation.SubscriptionID
	}
	return b.Confirmation.SubscriptionID
}

// EquipmentRefs are the booking's containers: the eBL's, or those a truck
// leg collects. An order names none: its legs do.
func (b *Booking) EquipmentRefs() []string {
	switch {
	case b.Road != nil:
		return slices.Clone(b.Road.Request.EquipmentReferences)
	case b.Order != nil:
		return nil
	}
	return b.Confirmation.EquipmentRefs()
}

// Places are where the booking starts and ends (UN/LOCODEs): the ports of
// loading and discharge, a truck leg's pickup and delivery, or an order's
// places of receipt and delivery.
func (b *Booking) Places() (from, to string) {
	switch {
	case b.Road != nil:
		return b.Road.Request.Pickup.UNLocationCode, b.Road.Request.Delivery.UNLocationCode
	case b.Order != nil:
		return b.Order.Request.PlaceOfReceipt.UNLocationCode, b.Order.Request.PlaceOfDelivery.UNLocationCode
	}
	return b.Request.PortOfLoading, b.Request.PortOfDischarge
}

// ShipperRef is the shipper's own reference for the booking.
func (b *Booking) ShipperRef() string {
	switch {
	case b.Road != nil:
		return b.Road.Request.ShipperRef
	case b.Order != nil:
		return b.Order.Request.ShipperRef
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
	sv Saver // nil: in memory only
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
	b.Operator, b.Ref = k.operator, k.ref
	if s.sv != nil {
		if err := s.sv.Put(k.String(), b); err != nil {
			return err
		}
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
	if s.sv != nil {
		if err := s.sv.Put(k.String(), b); err != nil {
			return Booking{}, err
		}
	}
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
	// The road and order bodies nest deeply (parties, addresses, stops); a
	// JSON round trip copies all of it, and they're JSON by nature. (Plain
	// structs of strings, numbers and slices: they always marshal and
	// unmarshal.)
	if b.Road != nil {
		b.Road = roundTrip(b.Road)
	}
	if b.Order != nil {
		b.Order = roundTrip(b.Order)
	}
	return b
}

func roundTrip[T any](v *T) *T {
	raw, _ := json.Marshal(v)
	out := new(T)
	_ = json.Unmarshal(raw, out)
	return out
}
