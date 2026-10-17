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

package intents

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/internal/logsafe"
)

// The forwarding intents: a door-to-door order from a shipper to a freight
// forwarder, who plans the journey and books each leg itself
// (docs/proposals/door-to-door-forwarder.md). Their bodies are ULP's own:
// the ULP payload specification has no forwarding order. They share the
// road bodies' parties, addresses and windows.
const (
	ForwardingRequest      = "ulp.forwarding.request"
	ForwardingConfirmation = "ulp.forwarding.confirmation"
)

// Bounds.
const MaxLegs = 8

// Place types: a door (a warehouse, a factory, found by its address) or a
// port terminal.
const (
	PlaceDoor = "door"
	PlacePort = "port"
)

// StatusPending is an order received and given its reference, whose plan
// a later ulp.forwarding.confirmation brings (confirmed or rejected).
const StatusPending = "pending"

// Transport modes of a planned leg.
const (
	ModeRoad  = "road"
	ModeOcean = "ocean"
	ModeRail  = "rail"
)

// incotermsRules are the eleven rules of Incoterms 2020.
var incotermsRules = map[string]bool{
	"EXW": true, "FCA": true, "CPT": true, "CIP": true, "DAP": true, "DPU": true, "DDP": true,
	"FAS": true, "FOB": true, "CFR": true, "CIF": true,
}

// Incoterms is the order's trade term: a rule of an Incoterms version, at
// a named place. It says who arranges and pays for which part of the
// journey, so it travels with the order.
type Incoterms struct {
	Rule       string `json:"rule"`
	Version    string `json:"version"`
	NamedPlace string `json:"named_place"`
}

// Place is where an order starts or ends: a door (found by its address) or
// a port terminal (by its facility), its UN/LOCODE, and when the cargo is
// ready there (earliest) or must have arrived (latest).
type Place struct {
	Type           string   `json:"type"` // door or port
	Facility       string   `json:"facility,omitempty"`
	FacilityCode   string   `json:"facility_code,omitempty"`
	UNLocationCode string   `json:"un_location_code"`
	Address        *Address `json:"address,omitempty"`
	// PlusCode pins the place where an address can't, as a road stop's.
	PlusCode string `json:"plus_code,omitempty"`
	Earliest string `json:"earliest,omitempty"` // UTC, whole seconds
	Latest   string `json:"latest,omitempty"`
}

// ForwardingRequestBody is the body of ulp.forwarding.request: move these
// containers from one place to another under this trade term. How (which
// legs, which carriers) is the forwarder's to plan.
type ForwardingRequestBody struct {
	ShipperRef      string        `json:"shipper_reference"`
	Incoterms       Incoterms     `json:"incoterms"`
	Shipper         Party         `json:"shipper"`
	Consignee       Party         `json:"consignee"`
	PlaceOfReceipt  Place         `json:"place_of_receipt"`
	PlaceOfDelivery Place         `json:"place_of_delivery"`
	Equipment       []Equipment   `json:"containers"`
	Cargo           Cargo         `json:"cargo"`
	Subscription    *Subscription `json:"subscription,omitempty"`
}

// PlannedLeg is one leg of the journey a forwarder plans: its mode, from
// and to (UN/LOCODEs), and optionally the carrier's name, the vessel and
// the planned times. It says what the customer is told, not what the
// forwarder pays.
type PlannedLeg struct {
	Mode             string `json:"mode"` // road, ocean or rail
	From             string `json:"from"`
	To               string `json:"to"`
	Carrier          string `json:"carrier,omitempty"`
	Vessel           string `json:"vessel,omitempty"`
	PlannedDeparture string `json:"planned_departure,omitempty"`
	PlannedArrival   string `json:"planned_arrival,omitempty"`
}

// ForwardingConfirmationBody is the body of ulp.forwarding.confirmation:
// the forwarder's reference for the order and, if it takes it, the
// journey it plans and when the cargo will be delivered. The answer to
// the order may be "pending" (received; the plan follows): the forwarder
// then sends a second confirmation, confirmed or rejected, in reply to
// the same order.
type ForwardingConfirmationBody struct {
	OrderRef        string       `json:"forwarder_reference"`
	Status          string       `json:"status"` // confirmed or rejected
	Legs            []PlannedLeg `json:"legs,omitempty"`
	PlannedDelivery string       `json:"planned_delivery,omitempty"`
	SubscriptionID  string       `json:"subscription_id,omitempty"`
	Messages        []Message    `json:"messages,omitempty"`
}

// Intent implements Body.
func (*ForwardingRequestBody) Intent() string { return ForwardingRequest }

// Intent implements Body.
func (*ForwardingConfirmationBody) Intent() string { return ForwardingConfirmation }

func (i *Incoterms) validate() error {
	var errs []error
	if !incotermsRules[i.Rule] {
		errs = append(errs, fmt.Errorf("incoterms.rule: %s is not an Incoterms 2020 rule", logsafe.Quote(i.Rule)))
	}
	if i.Version != "2020" {
		errs = append(errs, fmt.Errorf("incoterms.version: %s is not 2020", logsafe.Quote(i.Version)))
	}
	return errors.Join(append(errs, text("incoterms.named_place", i.NamedPlace, 128, false))...)
}

func (p *Place) validate(field string) error {
	errs := []error{
		match(field+".un_location_code", p.UNLocationCode, unLocodePattern),
		window(field, p.Earliest, p.Latest),
	}
	switch p.Type {
	case PlaceDoor:
		// A door is found by its address; a name is optional.
		if p.Address == nil {
			errs = append(errs, fmt.Errorf("%s: a door has an address", field))
		}
	case PlacePort:
		if p.Facility == "" {
			errs = append(errs, fmt.Errorf("%s: a port names its terminal (facility)", field))
		}
	default:
		errs = append(errs, fmt.Errorf("%s.type: %s is not %s or %s", field, logsafe.Quote(p.Type), PlaceDoor, PlacePort))
	}
	if p.Facility != "" {
		errs = append(errs, text(field+".facility", p.Facility, 128, false))
	}
	if p.PlusCode != "" {
		errs = append(errs, match(field+".plus_code", p.PlusCode, plusCodePattern))
	}
	if p.FacilityCode != "" {
		errs = append(errs, match(field+".facility_code", p.FacilityCode, facilityCodePattern))
	}
	if p.Address != nil {
		errs = append(errs, p.Address.validate(field+".address"), countryOf(field, p.UNLocationCode, p.Address))
	}
	return errors.Join(errs...)
}

// samePlace reports whether two places are the same: the same facility
// code, the same facility at the same UN/LOCODE, or the same address.
func samePlace(a, b Place) bool {
	if a.FacilityCode != "" && strings.EqualFold(a.FacilityCode, b.FacilityCode) {
		return true
	}
	if a.Facility != "" && a.UNLocationCode == b.UNLocationCode && strings.EqualFold(strings.TrimSpace(a.Facility), strings.TrimSpace(b.Facility)) {
		return true
	}
	return a.Address != nil && b.Address != nil && sameAddress(*a.Address, *b.Address)
}

func sameAddress(a, b Address) bool {
	norm := func(s string) string { return strings.Join(strings.Fields(strings.ToLower(s)), " ") }
	return a.Country == b.Country && norm(a.Street) == norm(b.Street) && norm(a.Locality) == norm(b.Locality) && norm(a.PostalCode) == norm(b.PostalCode)
}

// Validate implements Body.
func (b *ForwardingRequestBody) Validate() error {
	errs := []error{
		ref("shipper_reference", b.ShipperRef),
		b.Incoterms.validate(),
		b.Shipper.validate("shipper"),
		b.Consignee.validate("consignee"),
		b.PlaceOfReceipt.validate("place_of_receipt"),
		b.PlaceOfDelivery.validate("place_of_delivery"),
		b.Cargo.validate(),
	}
	if samePlace(b.PlaceOfReceipt, b.PlaceOfDelivery) {
		errs = append(errs, errors.New("place_of_delivery is place_of_receipt"))
	}
	// The cargo can't have to arrive before it's ready.
	if b.PlaceOfReceipt.Earliest != "" && b.PlaceOfDelivery.Latest != "" {
		from, err1 := time.Parse(envelope.TimeLayout, b.PlaceOfReceipt.Earliest)
		to, err2 := time.Parse(envelope.TimeLayout, b.PlaceOfDelivery.Latest)
		if err1 == nil && err2 == nil && to.Before(from) {
			errs = append(errs, errors.New("place_of_delivery.latest is before place_of_receipt.earliest"))
		}
	}
	if len(b.Equipment) == 0 || len(b.Equipment) > MaxEquipmentLines {
		errs = append(errs, fmt.Errorf("containers has %d lines, want 1-%d", len(b.Equipment), MaxEquipmentLines))
	}
	for i, e := range b.Equipment {
		errs = append(errs, e.validate(fmt.Sprintf("containers[%d]", i)))
	}
	if b.Subscription != nil {
		errs = append(errs, b.Subscription.validate())
	}
	return errors.Join(errs...)
}

func (l *PlannedLeg) validate(field string) error {
	errs := []error{
		match(field+".from", l.From, unLocodePattern),
		match(field+".to", l.To, unLocodePattern),
	}
	// Their order is legs' to check, with the rest of the journey.
	if l.PlannedDeparture != "" {
		_, err := timestamp(field+".planned_departure", l.PlannedDeparture)
		errs = append(errs, err)
	}
	if l.PlannedArrival != "" {
		_, err := timestamp(field+".planned_arrival", l.PlannedArrival)
		errs = append(errs, err)
	}
	switch l.Mode {
	case ModeRoad, ModeRail:
		if l.Vessel != "" {
			errs = append(errs, fmt.Errorf("%s: a %s leg has no vessel", field, l.Mode))
		}
	case ModeOcean:
	default:
		errs = append(errs, fmt.Errorf("%s.mode: %s is not %s, %s or %s", field, logsafe.Quote(l.Mode), ModeRoad, ModeOcean, ModeRail))
	}
	if l.Carrier != "" {
		errs = append(errs, text(field+".carrier", l.Carrier, 128, false))
	}
	if l.Vessel != "" {
		errs = append(errs, text(field+".vessel", l.Vessel, 128, false))
	}
	return errors.Join(errs...)
}

// Validate implements Body.
func (b *ForwardingConfirmationBody) Validate() error {
	errs := []error{ref("forwarder_reference", b.OrderRef)}
	switch b.Status {
	case StatusConfirmed:
		if len(b.Legs) == 0 || len(b.Legs) > MaxLegs {
			errs = append(errs, fmt.Errorf("legs has %d, want 1-%d", len(b.Legs), MaxLegs))
		}
		if b.PlannedDelivery == "" {
			errs = append(errs, errors.New("a confirmation plans the delivery"))
		} else if _, err := timestamp("planned_delivery", b.PlannedDelivery); err != nil {
			errs = append(errs, err)
		}
	case StatusPending:
		// Received, with its reference (and subscription): the plan
		// follows in a later confirmation.
		if len(b.Legs) > 0 || b.PlannedDelivery != "" {
			errs = append(errs, errors.New("a pending order carries no plan yet"))
		}
	case StatusRejected:
		if len(b.Legs) > 0 || b.PlannedDelivery != "" || b.SubscriptionID != "" {
			errs = append(errs, errors.New("a rejection carries no plan or subscription"))
		}
	default:
		errs = append(errs, fmt.Errorf("status: %s is not %s, %s or %s", logsafe.Quote(b.Status), StatusPending, StatusConfirmed, StatusRejected))
	}
	if len(b.Legs) <= MaxLegs {
		errs = append(errs, legs(b.Legs, b.PlannedDelivery))
	}
	if b.SubscriptionID != "" {
		errs = append(errs, ref("subscription_id", b.SubscriptionID))
	}
	errs = append(errs, validateMessages(b.Messages))
	return errors.Join(errs...)
}

// FitsOrder holds a confirmed answer to the order it answers: the journey
// runs from the place of receipt to the place of delivery, and delivers
// no later than the order asked. The forwarder checks it before sending,
// and the shipper on receipt.
func FitsOrder(req *ForwardingRequestBody, conf *ForwardingConfirmationBody) error {
	if len(conf.Legs) == 0 {
		return errors.New("no journey")
	}
	var errs []error
	if from := req.PlaceOfReceipt.UNLocationCode; conf.Legs[0].From != from {
		errs = append(errs, fmt.Errorf("the journey starts at %s, not the place of receipt %s", conf.Legs[0].From, from))
	}
	if to := req.PlaceOfDelivery.UNLocationCode; conf.Legs[len(conf.Legs)-1].To != to {
		errs = append(errs, fmt.Errorf("the journey ends at %s, not the place of delivery %s", conf.Legs[len(conf.Legs)-1].To, to))
	}
	if latest, ok := parsedTime(req.PlaceOfDelivery.Latest); ok {
		if planned, ok := parsedTime(conf.PlannedDelivery); ok && planned.After(latest) {
			errs = append(errs, fmt.Errorf("planned_delivery %s is after the latest asked for (%s)", conf.PlannedDelivery, req.PlaceOfDelivery.Latest))
		}
	}
	if earliest, ok := parsedTime(req.PlaceOfReceipt.Earliest); ok {
		if dep, ok := parsedTime(conf.Legs[0].PlannedDeparture); ok && dep.Before(earliest) {
			errs = append(errs, fmt.Errorf("the journey departs %s, before the cargo is ready (%s)", conf.Legs[0].PlannedDeparture, req.PlaceOfReceipt.Earliest))
		}
	}
	return errors.Join(errs...)
}

func parsedTime(s string) (time.Time, bool) {
	t, err := time.Parse(envelope.TimeLayout, s)
	return t, err == nil
}

// legs checks a planned journey: each leg valid and starting where the one
// before ended, and every planned time (each leg's departure, then its
// arrival) no earlier than the one before it and no later than the
// planned delivery. A leg without times takes nothing out of the order.
func legs(ls []PlannedLeg, delivery string) error {
	var errs []error
	end, _ := time.Parse(envelope.TimeLayout, delivery)
	var last time.Time // the latest planned time so far, at lastAt
	var lastAt string
	for i, l := range ls {
		at := fmt.Sprintf("legs[%d]", i)
		errs = append(errs, l.validate(at))
		if l.From == l.To && l.Mode != ModeRoad {
			// A truck may move a container across one town; a ship or a
			// train goes somewhere else.
			errs = append(errs, fmt.Errorf("%s: the %s leg starts and ends at %s", at, l.Mode, l.From))
		}
		if i > 0 && l.From != ls[i-1].To {
			errs = append(errs, fmt.Errorf("%s starts at %s, not where legs[%d] ends (%s)", at, l.From, i-1, ls[i-1].To))
		}
		for _, pt := range []struct{ name, value string }{{"planned_departure", l.PlannedDeparture}, {"planned_arrival", l.PlannedArrival}} {
			t, err := time.Parse(envelope.TimeLayout, pt.value)
			if err != nil {
				continue // absent, or malformed (the leg's own check says so)
			}
			if !last.IsZero() && t.Before(last) {
				errs = append(errs, fmt.Errorf("%s.%s is before %s", at, pt.name, lastAt))
			}
			if !end.IsZero() && t.After(end) {
				errs = append(errs, fmt.Errorf("%s.%s is after planned_delivery", at, pt.name))
			}
			last, lastAt = t, at+"."+pt.name
		}
	}
	return errors.Join(errs...)
}
