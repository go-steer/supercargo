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
	"net/url"

	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/internal/logsafe"
)

// BookingRequestBody is the body of ulp.ocean.booking.request: what a
// shipper knows when it asks for a booking. It is ULP's own body, in the
// ULP payload specification's snake_case style, reusing its member names
// where it has them (port_of_loading, port_of_discharge, shipper_name,
// equipment_size_type, gross_weight_kg, service_contract_number). (pf's
// ulp.ocean.booking body is a DCSA eBL, which names the vessel and the
// transport document: the carrier's to issue, so it comes back in the
// confirmation.) The
// shipper's required delivery date is deliberately not part of it: that
// is private context, used only by the shipper's own model.
type BookingRequestBody struct {
	ShipperRef            string        `json:"shipper_reference"`
	ShipperName           string        `json:"shipper_name"`
	PortOfLoading         string        `json:"port_of_loading"`
	PortOfDischarge       string        `json:"port_of_discharge"`
	Equipment             []Equipment   `json:"containers"`
	Cargo                 Cargo         `json:"cargo"`
	EarliestDeparture     string        `json:"earliest_departure"`
	ServiceContractNumber string        `json:"service_contract_number,omitempty"`
	Consignee             *Consignee    `json:"consignee,omitempty"`
	Subscription          *Subscription `json:"subscription,omitempty"`
}

// Location is a place, identified by its UN/LOCODE, with a display name.
// It is not part of any body: nodes name ports for people.
type Location struct {
	UNLocode string `json:"unLocode"`
	Name     string `json:"name"`
}

// Equipment is a number of containers of one ISO 6346 size-type, with
// their gross weight each.
type Equipment struct {
	ISOType       string  `json:"equipment_size_type"`
	Count         int     `json:"count"`
	GrossWeightKg float64 `json:"gross_weight_kg"`
}

// Cargo describes what is shipped.
type Cargo struct {
	Description string `json:"description"`
	HSCode      string `json:"hs_code"`
}

// Consignee is the receiving party. Domain makes it a ULP party that can be
// granted delegated access later (a ULP extension: pf names only a
// consignee_name).
type Consignee struct {
	Name   string `json:"name"`
	Domain string `json:"domain"`
}

// Subscription asks the operator to push the listed intents for the new
// booking to Endpoint (a ULP extension).
type Subscription struct {
	Endpoint string   `json:"endpoint"`
	Intents  []string `json:"intents"`
}

// Limits on booking request lists, and a container's gross weight.
const (
	MaxEquipmentLines = 20
	MaxEquipmentCount = 999
	MaxGrossWeightKg  = 40000
)

// Intent implements Body.
func (*BookingRequestBody) Intent() string { return BookingRequest }

// Validate implements Body.
func (b *BookingRequestBody) Validate() error {
	errs := []error{
		ref("shipper_reference", b.ShipperRef),
		text("shipper_name", b.ShipperName, 128, false),
		match("port_of_loading", b.PortOfLoading, unLocodePattern),
		match("port_of_discharge", b.PortOfDischarge, unLocodePattern),
		b.Cargo.validate(),
		date("earliest_departure", b.EarliestDeparture),
	}
	if b.PortOfLoading == b.PortOfDischarge {
		errs = append(errs, errors.New("port_of_discharge is port_of_loading"))
	}
	if b.ServiceContractNumber != "" {
		errs = append(errs, ref("service_contract_number", b.ServiceContractNumber))
	}
	if len(b.Equipment) == 0 || len(b.Equipment) > MaxEquipmentLines {
		errs = append(errs, fmt.Errorf("containers has %d lines, want 1-%d", len(b.Equipment), MaxEquipmentLines))
	}
	for i, e := range b.Equipment {
		errs = append(errs, e.validate(fmt.Sprintf("containers[%d]", i)))
	}
	if b.Consignee != nil {
		errs = append(errs, text("consignee.name", b.Consignee.Name, 128, false))
		if !envelope.ValidDomain(b.Consignee.Domain) {
			errs = append(errs, fmt.Errorf("consignee.domain: %s is not a lowercase DNS domain", logsafe.Quote(b.Consignee.Domain)))
		}
	}
	if b.Subscription != nil {
		errs = append(errs, b.Subscription.validate())
	}
	return errors.Join(errs...)
}

func (e Equipment) validate(field string) error {
	errs := []error{match(field+".equipment_size_type", e.ISOType, isoTypePattern)}
	if e.Count < 1 || e.Count > MaxEquipmentCount {
		errs = append(errs, fmt.Errorf("%s.count is %d, want 1-%d", field, e.Count, MaxEquipmentCount))
	}
	errs = append(errs, weight(field+".gross_weight_kg", e.GrossWeightKg))
	return errors.Join(errs...)
}

// weight checks a container's gross weight: positive, at most
// MaxGrossWeightKg (NaN fails both).
func weight(field string, w float64) error {
	if !(w > 0 && w <= MaxGrossWeightKg) {
		return fmt.Errorf("%s is %v, want more than 0 and at most %d", field, w, MaxGrossWeightKg)
	}
	return nil
}

func (c Cargo) validate() error {
	return errors.Join(
		text("cargo.description", c.Description, 512, false),
		match("cargo.hs_code", c.HSCode, hsCodePattern),
	)
}

// subscribable are the intents a subscription may ask for.
var subscribable = map[string]bool{TnTEvent: true, TnTAlert: true}

func (s *Subscription) validate() error {
	var errs []error
	if err := s.CheckEndpointForm(); err != nil {
		errs = append(errs, err)
	}
	if len(s.Intents) == 0 {
		errs = append(errs, errors.New("subscription.intents is empty"))
	}
	seen := map[string]bool{}
	for _, in := range s.Intents {
		if !subscribable[in] {
			errs = append(errs, fmt.Errorf("subscription.intents: %s cannot be subscribed to", logsafe.Quote(in)))
		}
		if seen[in] {
			errs = append(errs, fmt.Errorf("subscription.intents: %s is listed twice", logsafe.Quote(in)))
		}
		seen[in] = true
	}
	return errors.Join(errs...)
}

// CheckEndpointForm checks that Endpoint is an absolute http(s) URL whose
// host is a canonical domain (lowercase, no IP literal, no trailing dot)
// with no explicit port, and with nothing that could hide where it points
// (userinfo, query, fragment). Canonical matters twice: the authorization
// rule compares EndpointHost with the sender's domain, and delivery looks
// the host up as written. Whether plain http is acceptable is the node's
// TLS policy; where the host resolves is the delivery path's check.
func (s *Subscription) CheckEndpointForm() error {
	u, err := url.Parse(s.Endpoint)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host != u.Hostname() ||
		!envelope.ValidDomain(u.Hostname()) || u.User != nil || u.Fragment != "" || u.RawQuery != "" || u.Opaque != "" {
		return fmt.Errorf("subscription.endpoint: %s is not an absolute http(s) URL on a lowercase domain, without port, userinfo, query or fragment", logsafe.Quote(s.Endpoint))
	}
	if len(s.Endpoint) > 512 {
		return errors.New("subscription.endpoint is over 512 characters")
	}
	return nil
}

// EndpointHost is the host of Endpoint, for the authorization rule that it
// must be the subscriber's own domain. Compare it only after
// CheckEndpointForm has passed (Decode runs it), which makes it canonical.
func (s *Subscription) EndpointHost() string {
	u, err := url.Parse(s.Endpoint)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// Booking confirmation statuses.
const (
	StatusConfirmed           = "confirmed"
	StatusRejected            = "rejected"
	StatusAlternativeProposed = "alternative_proposed" // reserved
)

// DialectDCSAEBL is the payload schema of a confirmation's eBL: the ULP
// payload specification's DCSA ocean container document.
var DialectDCSAEBL = envelope.Dialect{Standard: "DCSA_OCEAN_CONTAINER", Version: "2.0.0", SchemaURL: "https://dcsa.org/standards/ebill-of-lading"}

// BookingConfirmationBody is the body of ulp.ocean.booking.confirmation,
// the operator's answer to a booking request. A confirmed booking carries
// its eBL, the standard payload, which names the vessel, the transport
// document and the containers; the rest is ULP's.
type BookingConfirmationBody struct {
	BookingRef       string    `json:"carrier_booking_reference"`
	Status           string    `json:"status"`
	PlannedDeparture string    `json:"planned_departure,omitempty"`
	PlannedArrival   string    `json:"planned_arrival,omitempty"`
	SubscriptionID   string    `json:"subscription_id,omitempty"`
	Messages         []Message `json:"messages,omitempty"`
	EBL              *EBL      `json:"ebl,omitempty"`
}

// EBL is the DCSA electronic bill of lading, as the ULP payload
// specification defines it.
type EBL struct {
	TransportDocumentReference string              `json:"transport_document_reference"`
	VesselName                 string              `json:"vessel_name"`
	VoyageNumber               string              `json:"voyage_number"`
	PortOfLoading              string              `json:"port_of_loading"`
	PortOfDischarge            string              `json:"port_of_discharge"`
	ShipperName                string              `json:"shipper_name"`
	ConsigneeName              string              `json:"consignee_name"`
	Containers                 []ShippingContainer `json:"containers"`
	ServiceContractNumber      string              `json:"service_contract_number,omitempty"`
	Status                     string              `json:"status"`
}

// ShippingContainer is one container on an eBL.
type ShippingContainer struct {
	ContainerNumber          string   `json:"container_number"`
	EquipmentSizeType        string   `json:"equipment_size_type"`
	SealNumber               string   `json:"seal_number"`
	GrossWeightKg            float64  `json:"gross_weight_kg"`
	IsReefer                 bool     `json:"is_reefer"`
	TargetTemperatureCelsius *float64 `json:"target_temperature_celsius,omitempty"`
}

// eBL statuses.
var eblStatuses = set("BOOKED", "LOADED", "IN_TRANSIT", "DISCHARGED", "DELIVERED")

// Message is a coded note attached to a response.
type Message struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// Vessel returns the vessel and voyage of a confirmed booking (empty
// otherwise).
func (b *BookingConfirmationBody) Vessel() (name, voyage string) {
	if b.EBL == nil {
		return "", ""
	}
	return b.EBL.VesselName, b.EBL.VoyageNumber
}

// EquipmentRefs returns the container numbers of a confirmed booking.
func (b *BookingConfirmationBody) EquipmentRefs() []string {
	if b.EBL == nil {
		return nil
	}
	out := make([]string, 0, len(b.EBL.Containers))
	for _, c := range b.EBL.Containers {
		out = append(out, c.ContainerNumber)
	}
	return out
}

// TransportDocumentReference returns the eBL's reference (empty if none).
func (b *BookingConfirmationBody) TransportDocumentReference() string {
	if b.EBL == nil {
		return ""
	}
	return b.EBL.TransportDocumentReference
}

// Intent implements Body.
func (*BookingConfirmationBody) Intent() string { return BookingConfirmation }

// Validate implements Body. A confirmed booking carries its eBL and its
// planned departure and arrival, in that order; any other carries no eBL.
func (b *BookingConfirmationBody) Validate() error {
	errs := []error{ref("carrier_booking_reference", b.BookingRef)}
	switch b.Status {
	case StatusConfirmed:
		if b.EBL == nil {
			errs = append(errs, errors.New("ebl is required when confirmed"))
		}
		dep, err1 := timestamp("planned_departure", b.PlannedDeparture)
		arr, err2 := timestamp("planned_arrival", b.PlannedArrival)
		errs = append(errs, err1, err2)
		if err1 == nil && err2 == nil && !arr.After(dep) {
			errs = append(errs, errors.New("planned_arrival is not after planned_departure"))
		}
	case StatusRejected, StatusAlternativeProposed:
		if b.EBL != nil {
			errs = append(errs, fmt.Errorf("ebl is only for a confirmed booking, not %s", b.Status))
		}
		for field, v := range map[string]string{"planned_departure": b.PlannedDeparture, "planned_arrival": b.PlannedArrival} {
			if v != "" {
				_, err := timestamp(field, v)
				errs = append(errs, err)
			}
		}
	default:
		errs = append(errs, fmt.Errorf("status: %s is not confirmed, rejected or alternative_proposed", logsafe.Quote(b.Status)))
	}
	if b.EBL != nil {
		errs = append(errs, b.EBL.validate())
	}
	if b.SubscriptionID != "" {
		errs = append(errs, ref("subscription_id", b.SubscriptionID))
	}
	errs = append(errs, validateMessages(b.Messages))
	return errors.Join(errs...)
}

func (e *EBL) validate() error {
	errs := []error{
		statusRef("ebl.transport_document_reference", e.TransportDocumentReference, 64, true),
		text("ebl.vessel_name", e.VesselName, 128, false),
		ref("ebl.voyage_number", e.VoyageNumber),
		match("ebl.port_of_loading", e.PortOfLoading, unLocodePattern),
		match("ebl.port_of_discharge", e.PortOfDischarge, unLocodePattern),
		text("ebl.shipper_name", e.ShipperName, 128, false),
		text("ebl.consignee_name", e.ConsigneeName, 128, false),
	}
	if !eblStatuses[e.Status] {
		errs = append(errs, fmt.Errorf("ebl.status: %s is not BOOKED, LOADED, IN_TRANSIT, DISCHARGED or DELIVERED", logsafe.Quote(e.Status)))
	}
	if e.ServiceContractNumber != "" {
		errs = append(errs, ref("ebl.service_contract_number", e.ServiceContractNumber))
	}
	if len(e.Containers) == 0 || len(e.Containers) > MaxEquipmentLines*MaxEquipmentCount {
		errs = append(errs, fmt.Errorf("ebl.containers has %d entries", len(e.Containers)))
	}
	seen := map[string]bool{}
	for i, c := range e.Containers {
		f := fmt.Sprintf("ebl.containers[%d]", i)
		errs = append(errs, match(f+".container_number", c.ContainerNumber, containerPattern),
			match(f+".equipment_size_type", c.EquipmentSizeType, isoTypePattern),
			ref(f+".seal_number", c.SealNumber), weight(f+".gross_weight_kg", c.GrossWeightKg))
		if t := c.TargetTemperatureCelsius; t != nil && !(*t >= -40 && *t <= 40) {
			errs = append(errs, fmt.Errorf("%s.target_temperature_celsius is %v, want -40 to 40", f, *t))
		}
		if t := c.TargetTemperatureCelsius; t != nil && !c.IsReefer {
			errs = append(errs, fmt.Errorf("%s has a set point but is not a reefer", f))
		}
		if seen[c.ContainerNumber] {
			errs = append(errs, fmt.Errorf("%s.container_number %s repeats", f, c.ContainerNumber))
		}
		seen[c.ContainerNumber] = true
	}
	return errors.Join(errs...)
}

// MaxMessages bounds the messages on one response.
const MaxMessages = 20

func validateMessages(ms []Message) error {
	if len(ms) > MaxMessages {
		return fmt.Errorf("messages has %d entries, over %d", len(ms), MaxMessages)
	}
	var errs []error
	for i, m := range ms {
		errs = append(errs,
			match(fmt.Sprintf("messages[%d].code", i), m.Code, codePattern),
			text(fmt.Sprintf("messages[%d].detail", i), m.Detail, 512, false))
	}
	return errors.Join(errs...)
}
