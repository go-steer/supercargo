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

	"github.com/go-steer/supercargo/internal/business"
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
