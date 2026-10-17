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

// Package backend is a node's integration surface on its other side: the
// ports through which its core reaches the company's own systems (a
// carrier's booking system, a shipper's ERP), and is reached by them
// (docs/proposals/backend-integration.md).
//
// A port is an interface the core calls. Each has adapters: in memory
// (the demo's built-in behaviour, and tests), and later REST, MCP or EDI.
// The rules don't change with the adapter:
//   - the core calls a port only after a partner's message is verified and
//     authorized, and only for the data the request needs;
//   - models never call a port; deterministic code does;
//   - what a port returns is validated before it is signed or sent.
package backend

import (
	"context"
	"time"

	"github.com/go-steer/supercargo/internal/business"
	"github.com/go-steer/supercargo/internal/geo"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/intents"
)

// Bookings is a carrier's booking decision: what its booking system or
// TMS owns. Given a verified, authorized request, it accepts the booking
// (assigning the reference, the schedule and the transport document) or
// declines it. The core stores the result and answers the shipper.
type Bookings interface {
	Decide(ctx context.Context, r BookingRequest) (*intents.BookingConfirmationBody, error)
}

// BookingRequest is a verified, authorized booking request.
type BookingRequest struct {
	Shipper  string // the requesting company's domain
	Operator string // this carrier's domain
	Body     *intents.BookingRequestBody
}

// RoadBookings is a road carrier's booking decision: its TMS plans a
// truck leg (assigning the reference, the pickup and delivery times and
// the documents the law requires) or declines it, and decides whether an
// amended leg can still be run.
type RoadBookings interface {
	Decide(ctx context.Context, r RoadBookingRequest) (*intents.RoadBookingConfirmationBody, error)
	Amend(ctx context.Context, r RoadAmendment) (*intents.RoadBookingConfirmationBody, error)
}

// RoadBookingRequest is a verified, authorized road booking request.
type RoadBookingRequest struct {
	Shipper  string // the requesting company's domain
	Operator string // this carrier's domain
	Body     *intents.RoadBookingRequestBody
}

// RoadAmendment is a verified, authorized amendment of a booked leg:
// Request is the leg as amended (the new windows in place), Current the
// confirmation it replaces. The answer keeps the booking's reference; a
// rejection leaves the booking as it was.
type RoadAmendment struct {
	Shipper   string
	Operator  string
	Request   *intents.RoadBookingRequestBody
	Current   *intents.RoadBookingConfirmationBody
	Amendment *intents.RoadBookingAmendmentBody
}

// RiskClearances is a road carrier's risk manager (in Brazil, the
// gerenciadora de risco its insurer requires): before a leg is accepted,
// it clears the vehicle and driver for the route and cargo, under a
// monitoring protocol. A leg it doesn't clear is refused.
type RiskClearances interface {
	Clear(ctx context.Context, r RiskRequest) (*Clearance, error)
}

// RiskRequest is a verified, authorized leg to clear.
type RiskRequest struct {
	BookingRef string
	Body       *intents.RoadBookingRequestBody
	Pickup     time.Time
}

// Clearance is the risk manager's answer: approved or not, under which
// protocol, for which vehicle and driver (the carrier's own people's
// data: it stays with the carrier).
type Clearance struct {
	Approved bool
	Protocol string
	Vehicle  string
	Driver   string
	Source   string
}

// TollVouchers issues a leg's toll voucher (in Brazil, the Vale-Pedágio
// obrigatório, paid ahead and not part of the freight).
type TollVouchers interface {
	Issue(ctx context.Context, r VoucherRequest) (*Voucher, error)
}

// VoucherRequest is a cleared leg's voucher to issue.
type VoucherRequest struct {
	BookingRef string
	Body       *intents.RoadBookingRequestBody
	Vehicle    string
}

// Voucher is an issued toll voucher.
type Voucher struct {
	Number string
	Source string
}

// BackOffice is a road booking system that says what its systems did for
// each leg it confirmed (optional, beside RoadBookings).
type BackOffice interface {
	BackOffice(ref string) []store.BackOfficeStep
}

// FreightFloors is a road leg's freight floor and tolls: in Brazil, the
// ANTT minimum freight (Lei nº 13.703/2018) and the toll plazas whose fares
// go to the carrier as a Vale-Pedágio voucher, from a routing service
// (QualP). It is for a forwarder to check a leg's agreed freight against
// before booking it; what it answers is a third party's, checked before
// use.
type FreightFloors interface {
	Floor(ctx context.Context, r FloorRequest) (*Floor, error)
}

// FloorRequest is a leg to price: its places in order (free text, e.g.
// "Santos, SP"), the vehicle's loaded axles, and the ANTT load type and
// table (A: the carrier's own tractor and trailer).
type FloorRequest struct {
	Locations []string
	Axles     int
	LoadType  string
	Table     string
}

// Floor is a leg's floor: the distance and duration, the toll plazas and
// their total for the axles asked, the ANTT floor for the distance (the
// freight, of which LoadUnload is the loading and unloading part, in
// reais), the resolution it comes from, and who answered ("qualp", or
// "simulated qualp").
type Floor struct {
	DistanceKm    float64       `json:"distanceKm"`
	Duration      time.Duration `json:"duration"`
	Tolls         []Toll        `json:"tolls"`
	TollTotal     float64       `json:"tollTotal"`
	Freight       float64       `json:"freight"`
	LoadUnload    float64       `json:"loadUnload"`
	Resolution    string        `json:"resolution"`
	ResolutionURL string        `json:"resolutionUrl,omitempty"`
	Source        string        `json:"source"`
}

// Toll is a toll plaza on a leg, with its fare for the leg's axles.
type Toll struct {
	Name     string  `json:"name"`
	Operator string  `json:"operator"`
	Road     string  `json:"road"`
	State    string  `json:"state"`
	Amount   float64 `json:"amount"`
}

// Weather is the weather at a place: for a road carrier, along a leg's
// corridor when it plans one (Google's Weather API, or the demo's mock).
// What it answers is a third party's, checked before use.
type Weather interface {
	// At is the forecast for the hour at holds, or the current
	// conditions when at is past or beyond the forecast.
	At(ctx context.Context, p geo.Point, at time.Time) (*Conditions, error)
}

// Conditions are the weather at a place and hour: the description in the
// language asked, the temperature, the chance of rain or snow, and who
// answered ("Google Weather API").
type Conditions struct {
	Description string    `json:"description"`
	TempC       float64   `json:"tempC"`
	RainChance  int       `json:"rainChance"` // percent
	At          time.Time `json:"at"`
	Forecast    bool      `json:"forecast"` // false: the current conditions
	Source      string    `json:"source"`
}

// DockAppointments is a receiving site's dock schedule (a yard or dock
// scheduling system): a shipment's appointment, the free slots, and moving
// an appointment to one. It is the shipper's own: what it holds never goes
// in an envelope. Only a person's confirmation moves an appointment.
type DockAppointments interface {
	Appointment(ctx context.Context, shipperRef string) (*Appointment, error)
	Slots(ctx context.Context, from time.Time, days int) ([]DockSlot, error)
	// Move moves appointment id from its start from (a stale draft's
	// from is refused) to a free slot.
	Move(ctx context.Context, id string, from time.Time, to DockSlot) (*Appointment, error)
}

// Appointment is a shipment's dock appointment at the receiving site.
type Appointment struct {
	ID         string    `json:"id"`
	ShipperRef string    `json:"shipperRef"`
	Site       string    `json:"site"`
	Dock       string    `json:"dock"`
	Start      time.Time `json:"start"`
	End        time.Time `json:"end"`
	Source     string    `json:"source"`
	// Policy says which standing policy last booked or moved it, and who
	// set it; empty when a person did.
	Policy string `json:"policy,omitempty"`
}

// DockBooker is a dock schedule a standing policy may act on (optional,
// beside DockAppointments): book a new appointment, and move one, each
// noting the policy; the free slots with an appointment's own counted
// free; and release one.
type DockBooker interface {
	Book(ctx context.Context, shipperRef, note string, to DockSlot) (*Appointment, error)
	MoveFor(ctx context.Context, id string, from time.Time, to DockSlot, note string) (*Appointment, error)
	SlotsFor(ctx context.Context, from time.Time, days int, except string) ([]DockSlot, error)
	Release(ctx context.Context, id string) error
}

// DockSlot is a free slot at a dock.
type DockSlot struct {
	Dock  string    `json:"dock"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// PrivateContext is a shipper's private context for its own shipments:
// what its ERP and warehouse systems own (delivery deadlines, receiving
// slots, notes). It is read by the shipper's own models and guards, and
// never put in an envelope.
type PrivateContext interface {
	// For is the context of one shipment, by the shipper's own reference.
	For(shipperRef string) (business.Shipment, bool)
	// CompanyNotes are standing notes that apply to every shipment.
	CompanyNotes() []string
	// Private lists the values a message to a partner must not mention.
	Private() []string
}
