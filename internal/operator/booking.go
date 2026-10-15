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

// Package operator is the carrier side of the demo: the services an
// operator's node runs behind the pipeline (docs/DESIGN.md, section A.8).
package operator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-steer/supercargo/internal/backend"
	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
)

// Voyage is the demo's single voyage, which every booking is placed on.
type Voyage struct {
	Vessel string
	Number string
	// Departure is the hour of day (UTC) the vessel departs, the day after
	// the earliest departure requested; Transit is how long it sails.
	DepartureHour int
	Transit       time.Duration
	// Transits override Transit for a port of discharge (UN/LOCODE).
	Transits map[string]time.Duration
}

// transitTo is how long the vessel sails to port.
func (v Voyage) transitTo(port string) time.Duration {
	if d, ok := v.Transits[port]; ok {
		return d
	}
	return v.Transit
}

// MaxContainers is the most containers one request may book. A larger
// request is answered "rejected": its confirmation would list more
// container numbers than fit in an envelope the shipper accepts.
const MaxContainers = 50

// BookingService handles ulp.ocean.booking.request: it authorizes the
// request, asks the carrier's booking system to decide (Backend), stores
// the booking and confirms it. The protocol's part is here; the business
// decision is the backend's (docs/proposals/backend-integration.md).
type BookingService struct {
	Bookings *store.Bookings
	// Deny lists domains whose requests are refused (docs/DESIGN.md, A.6:
	// "anyone may request; the operator may reject").
	Deny map[string]bool
	// Backend decides each booking: the carrier's booking system. Nil uses
	// VoyageBookings, built in memory from the fields below.
	Backend backend.Bookings
	// RefPrefix, ContainerPrefix and Voyage configure the in-memory
	// backend when Backend is nil (see VoyageBookings).
	RefPrefix       string
	ContainerPrefix string
	Voyage          Voyage
	Now             func() time.Time
	// OnBooked runs for each booking stored, before its confirmation is
	// sent: the hook the simulator starts a voyage from. Optional; it must
	// not block.
	OnBooked func(store.Booking)

	once sync.Once
}

// backend is the booking system to ask: Backend, or the in-memory one.
func (s *BookingService) backend() backend.Bookings {
	s.once.Do(func() {
		if s.Backend == nil {
			s.Backend = &VoyageBookings{RefPrefix: s.RefPrefix, ContainerPrefix: s.ContainerPrefix, Voyage: s.Voyage}
		}
	})
	return s.Backend
}

// Authorize implements node.Handler. It runs before anything is booked:
// the sender must not be denied, and a subscription must point at the
// sender's own domain, or the operator could be used to push shipment data
// to a third party (docs/DESIGN.md, A.6).
func (s *BookingService) Authorize(_ context.Context, req *node.Request) error {
	sender := req.Envelope.Header.Sender
	if s.Deny[sender] {
		return node.Reject(intents.CodeForbidden, "This operator does not accept bookings from the sender.", "sender is on the deny list")
	}
	b := req.Body.(*intents.BookingRequestBody)
	if b.Subscription != nil && b.Subscription.EndpointHost() != sender {
		return node.Reject(intents.CodeForbidden, "The subscription endpoint must be on the sender's own domain.",
			"subscription endpoint host "+b.Subscription.EndpointHost()+" is not "+sender)
	}
	return nil
}

// Handle implements node.Handler: the backend decides; a confirmed
// booking is stored, and either answer goes back to the shipper.
func (s *BookingService) Handle(ctx context.Context, req *node.Request) (*node.Reply, error) {
	b := req.Body.(*intents.BookingRequestBody)
	conf, err := s.backend().Decide(ctx, backend.BookingRequest{Shipper: req.Envelope.Header.Sender, Operator: req.Envelope.Header.Receiver, Body: b})
	if err != nil {
		return nil, err
	}
	if conf == nil {
		return nil, errors.New("operator: the booking system gave no answer")
	}
	if conf.Status == intents.StatusConfirmed && b.Subscription != nil && conf.SubscriptionID == "" {
		// Recorded with the booking; the Publisher pushes to it.
		conf.SubscriptionID = "SUB-" + strings.TrimPrefix(conf.BookingRef, s.backendPrefix())
	}
	// What the backend says, and what we added, is checked before anything
	// is stored or signed: a bug there must not become a live booking or a
	// malformed message to a partner.
	if err := conf.Validate(); err != nil {
		return nil, fmt.Errorf("operator: the booking system's answer: %w", err)
	}
	if conf.Status != intents.StatusConfirmed {
		// The protocol's answer to "no", rather than an error: the request
		// was valid, the operator just won't take it.
		return &node.Reply{BookingRef: conf.BookingRef, Body: conf}, nil
	}
	if err := s.Bookings.Add(store.Booking{
		Ref:          conf.BookingRef,
		Shipper:      req.Envelope.Header.Sender,
		Operator:     req.Envelope.Header.Receiver,
		Request:      *b,
		Confirmation: *conf,
		CreatedAt:    s.Now(),
	}); errors.Is(err, store.ErrFull) {
		return &node.Reply{BookingRef: conf.BookingRef, Body: &intents.BookingConfirmationBody{
			BookingRef: conf.BookingRef, Status: intents.StatusRejected,
			Messages: []intents.Message{{Code: "at_capacity", Detail: "This operator is not taking new bookings right now."}},
		}}, nil
	} else if err != nil {
		return nil, err
	}
	if s.OnBooked != nil {
		// The store's copy, which shares nothing with the reply.
		if stored, err := s.Bookings.Get(req.Envelope.Header.Receiver, conf.BookingRef); err == nil {
			s.OnBooked(stored)
		}
	}
	return &node.Reply{Body: conf, BookingRef: conf.BookingRef}, nil
}

// backendPrefix is the in-memory backend's reference prefix and dash
// ("OB-BKG-"), or "" for another backend.
func (s *BookingService) backendPrefix() string {
	if v, ok := s.backend().(*VoyageBookings); ok {
		return v.RefPrefix + "-"
	}
	return ""
}

// VoyageBookings is the in-memory booking system: it books every request
// on the configured voyage, assigning references, containers and seals
// from counters. It is what a carrier's TMS would decide; the demo's
// default.
type VoyageBookings struct {
	// RefPrefix starts each booking reference, e.g. "OB-BKG".
	RefPrefix string
	// ContainerPrefix is the operator's ISO 6346 owner code plus category,
	// e.g. "OCBU".
	ContainerPrefix string
	Voyage          Voyage

	mu     sync.Mutex
	next   int
	serial int
}

// Decide implements backend.Bookings: a reference, numbered and sealed
// containers, and the voyage's schedule; or a rejection for too many
// containers.
func (s *VoyageBookings) Decide(_ context.Context, r backend.BookingRequest) (*intents.BookingConfirmationBody, error) {
	b := r.Body
	earliest, err := time.Parse(intents.DateLayout, b.EarliestDeparture)
	if err != nil {
		return nil, err // Decode validated it; unreachable
	}
	containers := 0
	for _, e := range b.Equipment {
		containers += e.Count
	}

	s.mu.Lock()
	if s.next == 0 {
		s.next = 1042
	}
	n := s.next
	s.next++
	// Serials come from one counter, so no two containers share a number.
	firstSerial := s.serial
	if containers <= MaxContainers {
		s.serial += containers
	}
	s.mu.Unlock()
	ref := fmt.Sprintf("%s-%d", s.RefPrefix, n)

	if containers > MaxContainers {
		return &intents.BookingConfirmationBody{
			BookingRef: ref, Status: intents.StatusRejected,
			Messages: []intents.Message{{Code: "too_many_containers",
				Detail: fmt.Sprintf("This operator books at most %d containers per request.", MaxContainers)}},
		}, nil
	}

	departure := earliest.AddDate(0, 0, 1).Add(time.Duration(s.Voyage.DepartureHour) * time.Hour)
	// The eBL is the carrier's document: its reference, the vessel and
	// voyage, and a numbered, sealed container for each one asked for.
	consignee := "To order" // the eBL names a consignee; a request may not
	if b.Consignee != nil {
		consignee = b.Consignee.Name
	}
	ebl := &intents.EBL{
		TransportDocumentReference: fmt.Sprintf("%s-%d", s.documentPrefix(), n),
		VesselName:                 s.Voyage.Vessel, VoyageNumber: s.Voyage.Number,
		PortOfLoading: b.PortOfLoading, PortOfDischarge: b.PortOfDischarge,
		ShipperName: b.ShipperName, ConsigneeName: consignee, ServiceContractNumber: b.ServiceContractNumber,
		Status: "BOOKED",
	}
	i := 0
	for _, e := range b.Equipment {
		for range e.Count {
			serial := firstSerial + i
			ebl.Containers = append(ebl.Containers, intents.ShippingContainer{
				ContainerNumber: containerNumber(s.ContainerPrefix, serial), EquipmentSizeType: e.ISOType,
				SealNumber: fmt.Sprintf("%sS%07d", s.ContainerPrefix, serial), GrossWeightKg: e.GrossWeightKg,
				IsReefer: e.ISOType[2] == 'R'}) // ISO 6346: the type group's letter
			i++
		}
	}
	conf := &intents.BookingConfirmationBody{
		BookingRef:       ref,
		Status:           intents.StatusConfirmed,
		PlannedDeparture: departure.UTC().Format(envelope.TimeLayout),
		PlannedArrival:   departure.Add(s.Voyage.transitTo(b.PortOfDischarge)).UTC().Format(envelope.TimeLayout),
		EBL:              ebl,
	}
	if b.Subscription != nil {
		conf.SubscriptionID = fmt.Sprintf("SUB-%d", n)
	}
	return conf, nil
}

// documentPrefix prefixes transport document references: the booking
// prefix's first segment and "TD" ("OB-TD-1042" for "OB-BKG").
func (s *VoyageBookings) documentPrefix() string {
	head, _, _ := strings.Cut(s.RefPrefix, "-")
	return head + "-TD"
}
