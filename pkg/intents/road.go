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
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/internal/logsafe"
)

// The road intents: a truck leg booked with a carrier that has a node
// (docs/proposals/door-to-door-forwarder.md). Their bodies are ULP's own,
// in the dialect ULP_ROAD: metric, with international addresses and
// parties, and references to the documents a country's law requires (a
// Brazilian CT-e, a European e-CMR) rather than the documents themselves.
const (
	RoadBookingRequest      = "ulp.road.booking.request"
	RoadBookingConfirmation = "ulp.road.booking.confirmation"
	RoadBookingAmendment    = "ulp.road.booking.amendment"
)

// DialectULPRoad names the road bodies' schema: ULP's own, proposed where
// the ULP payload specification has no road dialect outside North America.
var DialectULPRoad = envelope.Dialect{Standard: "ULP_ROAD", Version: "0.1", SchemaURL: "https://github.com/go-steer/supercargo/blob/main/docs/DESIGN.md"}

// Bounds.
const (
	MaxIdentifiers = 8
	MaxDocuments   = 8
)

// Address is a postal address, structured internationally: an ISO 3166-1
// alpha-2 country, an optional region (a state, a Brazilian UF), the
// locality, a postal code and the street.
type Address struct {
	Street     string `json:"street"`
	PostalCode string `json:"postal_code,omitempty"`
	Locality   string `json:"locality"`
	Region     string `json:"region,omitempty"`
	Country    string `json:"country"`
}

// Identifier is a party's identifier under a scheme: an EU EORI number, a
// VAT number, a Brazilian CNPJ or CPF, a US EIN, a GS1 GLN.
type Identifier struct {
	Scheme string `json:"scheme"`
	Value  string `json:"value"`
}

// Party is a company or person on a booking: a name, optionally its ULP
// domain, its identifiers, and its address.
type Party struct {
	Name        string       `json:"name"`
	Domain      string       `json:"domain,omitempty"`
	Identifiers []Identifier `json:"identifiers,omitempty"`
	Address     *Address     `json:"address,omitempty"`
}

// Stop is where a truck picks up or delivers: a facility, its UN/LOCODE,
// its address, and the window to be there in.
type Stop struct {
	Facility       string   `json:"facility"`
	FacilityCode   string   `json:"facility_code,omitempty"`
	UNLocationCode string   `json:"un_location_code"`
	Address        *Address `json:"address,omitempty"`
	Earliest       string   `json:"earliest,omitempty"` // UTC, whole seconds
	Latest         string   `json:"latest,omitempty"`
}

// DocumentRef points at a document a law or a platform keeps: a CT-e or
// MDF-e by its 44-digit access key, an e-CMR or a MERCOSUR CRT by its
// number. The document itself never travels in a ULP body.
type DocumentRef struct {
	Type      string `json:"type"`
	AccessKey string `json:"access_key,omitempty"` // CT-e, MDF-e
	Number    string `json:"number,omitempty"`     // e-CMR, CRT, MIC/DTA
}

// RoadBookingRequestBody is the body of ulp.road.booking.request: a truck
// leg, from one stop to another, with the containers it carries.
type RoadBookingRequestBody struct {
	ShipperRef   string        `json:"shipper_reference"`
	Shipper      Party         `json:"shipper"`
	Consignee    *Party        `json:"consignee,omitempty"`
	Pickup       Stop          `json:"pickup"`
	Delivery     Stop          `json:"delivery"`
	Equipment    []Equipment   `json:"containers"`
	Cargo        Cargo         `json:"cargo"`
	Subscription *Subscription `json:"subscription,omitempty"`
}

// RoadBookingConfirmationBody is the body of ulp.road.booking.confirmation:
// the carrier's answer to a request or an amendment, with its planned
// times and the references of the documents it issued.
type RoadBookingConfirmationBody struct {
	BookingRef         string        `json:"carrier_booking_reference"`
	Status             string        `json:"status"` // confirmed or rejected
	PlannedPickup      string        `json:"planned_pickup,omitempty"`
	PlannedDelivery    string        `json:"planned_delivery,omitempty"`
	FiscalDocuments    []DocumentRef `json:"fiscal_documents,omitempty"`
	TransportDocuments []DocumentRef `json:"transport_documents,omitempty"`
	SubscriptionID     string        `json:"subscription_id,omitempty"`
	Messages           []Message     `json:"messages,omitempty"`
}

// RoadBookingAmendmentBody is the body of ulp.road.booking.amendment: new
// windows for a booked leg's pickup or delivery. It says what changes,
// never why: the reason is the shipper's business.
type RoadBookingAmendmentBody struct {
	BookingRef string  `json:"carrier_booking_reference"`
	Pickup     *Window `json:"pickup,omitempty"`
	Delivery   *Window `json:"delivery,omitempty"`
}

// Window is a stop's new time window.
type Window struct {
	Earliest string `json:"earliest,omitempty"`
	Latest   string `json:"latest,omitempty"`
}

// Intent implements Body.
func (*RoadBookingRequestBody) Intent() string { return RoadBookingRequest }

// Intent implements Body.
func (*RoadBookingConfirmationBody) Intent() string { return RoadBookingConfirmation }

// Intent implements Body.
func (*RoadBookingAmendmentBody) Intent() string { return RoadBookingAmendment }

var (
	countryPattern = regexp.MustCompile(`^[A-Z]{2}$`)
	regionPattern  = regexp.MustCompile(`^[A-Z0-9]{1,3}$`)
	postalPattern  = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9 -]{0,10}[A-Za-z0-9])?$`)
	// A CT-e or MDF-e access key: 44 characters, digits except the issuer's
	// CNPJ (positions 7-20), whose first 12 may be letters since Brazil's
	// alphanumeric CNPJ (from July 2026).
	accessKeyPattern = regexp.MustCompile(`^[0-9]{6}[0-9A-Z]{12}[0-9]{26}$`)
	// A document number is opaque: never put one in a path or a URL.
	docNumberPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9/_.-]{0,63}$`)
	identifierPattern = map[string]*regexp.Regexp{
		"EORI": regexp.MustCompile(`^[A-Z]{2}[A-Za-z0-9]{1,15}$`),
		"VAT":  regexp.MustCompile(`^[A-Z]{2}[A-Za-z0-9]{2,13}$`),
		// Bare, or punctuated in full; the first 12 may be letters since
		// the alphanumeric CNPJ.
		"BR:CNPJ": regexp.MustCompile(`^([0-9A-Z]{12}[0-9]{2}|[0-9A-Z]{2}\.[0-9A-Z]{3}\.[0-9A-Z]{3}/[0-9A-Z]{4}-[0-9]{2})$`),
		"BR:CPF":  regexp.MustCompile(`^([0-9]{11}|[0-9]{3}\.[0-9]{3}\.[0-9]{3}-[0-9]{2})$`),
		"US:EIN":  regexp.MustCompile(`^[0-9]{2}-?[0-9]{7}$`),
		"GLN":     regexp.MustCompile(`^[0-9]{13}$`),
	}
	// Document types, and whether each is referenced by an access key (a
	// Brazilian fiscal document) or a number.
	fiscalTypes    = map[string]bool{"CT-e": true, "MDF-e": true}
	transportTypes = map[string]bool{"e-CMR": true, "CMR": true, "CRT": true, "MIC/DTA": true}
)

func (a *Address) validate(field string) error {
	errs := []error{
		text(field+".street", a.Street, 128, false),
		text(field+".locality", a.Locality, 64, false),
		match(field+".country", a.Country, countryPattern),
	}
	if a.PostalCode != "" {
		errs = append(errs, match(field+".postal_code", a.PostalCode, postalPattern))
	}
	if a.Region != "" {
		errs = append(errs, match(field+".region", a.Region, regionPattern))
	}
	return errors.Join(errs...)
}

func (p *Party) validate(field string) error {
	errs := []error{text(field+".name", p.Name, 128, false)}
	if p.Domain != "" && !envelope.ValidDomain(p.Domain) {
		errs = append(errs, fmt.Errorf("%s.domain: %s is not a lowercase DNS domain", field, logsafe.Quote(p.Domain)))
	}
	if len(p.Identifiers) > MaxIdentifiers {
		errs = append(errs, fmt.Errorf("%s.identifiers has %d, want at most %d", field, len(p.Identifiers), MaxIdentifiers))
	}
	seen := map[string]bool{}
	for i, id := range p.Identifiers {
		at := fmt.Sprintf("%s.identifiers[%d]", field, i)
		re, ok := identifierPattern[id.Scheme]
		switch {
		case !ok:
			errs = append(errs, fmt.Errorf("%s.scheme: %s is not a known scheme", at, logsafe.Quote(id.Scheme)))
		case !re.MatchString(id.Value):
			errs = append(errs, fmt.Errorf("%s.value is not a %s", at, id.Scheme))
		case seen[id.Scheme]:
			errs = append(errs, fmt.Errorf("%s: a second %s", at, id.Scheme))
		}
		seen[id.Scheme] = true
	}
	if p.Address != nil {
		errs = append(errs, p.Address.validate(field+".address"))
	}
	return errors.Join(errs...)
}

// window checks an optional earliest/latest pair: whole-second UTC, and
// not ending before it starts.
func window(field, earliest, latest string) error {
	var errs []error
	var from, to time.Time
	var err error
	if earliest != "" {
		if from, err = timestamp(field+".earliest", earliest); err != nil {
			errs = append(errs, err)
		}
	}
	if latest != "" {
		if to, err = timestamp(field+".latest", latest); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 && earliest != "" && latest != "" && to.Before(from) {
		errs = append(errs, fmt.Errorf("%s.latest is before %s.earliest", field, field))
	}
	return errors.Join(errs...)
}

func (s *Stop) validate(field string) error {
	errs := []error{
		text(field+".facility", s.Facility, 128, false),
		match(field+".un_location_code", s.UNLocationCode, unLocodePattern),
		window(field, s.Earliest, s.Latest),
	}
	if s.FacilityCode != "" {
		errs = append(errs, match(field+".facility_code", s.FacilityCode, facilityCodePattern))
	}
	if s.Address != nil {
		errs = append(errs, s.Address.validate(field+".address"))
	}
	return errors.Join(errs...)
}

func documents(field string, docs []DocumentRef, types map[string]bool) error {
	if len(docs) > MaxDocuments {
		return fmt.Errorf("%s has %d, want at most %d", field, len(docs), MaxDocuments)
	}
	var errs []error
	seen := map[DocumentRef]bool{}
	for i, d := range docs {
		at := fmt.Sprintf("%s[%d]", field, i)
		if seen[d] {
			errs = append(errs, fmt.Errorf("%s: the same document twice", at))
		}
		seen[d] = true
		switch {
		case !types[d.Type]:
			errs = append(errs, fmt.Errorf("%s.type: %s is not one of %v", at, logsafe.Quote(d.Type), slices.Sorted(maps.Keys(types))))
		case fiscalTypes[d.Type]:
			// A Brazilian fiscal document is found by its access key.
			if d.Number != "" || !accessKeyPattern.MatchString(d.AccessKey) {
				errs = append(errs, fmt.Errorf("%s: a %s is referenced by its 44-digit access_key, and nothing else", at, d.Type))
			}
		default:
			if d.AccessKey != "" || !docNumberPattern.MatchString(d.Number) {
				errs = append(errs, fmt.Errorf("%s: a %s is referenced by its number, and nothing else", at, d.Type))
			}
		}
	}
	return errors.Join(errs...)
}

// sameStop reports whether two stops are the same place: the same
// facility code, or the same UN/LOCODE and facility name (ignoring case
// and surrounding space).
func sameStop(a, b Stop) bool {
	if a.FacilityCode != "" && a.FacilityCode == b.FacilityCode {
		return true
	}
	return a.UNLocationCode == b.UNLocationCode && strings.EqualFold(strings.TrimSpace(a.Facility), strings.TrimSpace(b.Facility))
}

// Validate implements Body.
func (b *RoadBookingRequestBody) Validate() error {
	errs := []error{
		ref("shipper_reference", b.ShipperRef),
		b.Shipper.validate("shipper"),
		b.Pickup.validate("pickup"),
		b.Delivery.validate("delivery"),
		b.Cargo.validate(),
	}
	if b.Consignee != nil {
		errs = append(errs, b.Consignee.validate("consignee"))
	}
	if sameStop(b.Pickup, b.Delivery) {
		errs = append(errs, errors.New("delivery is the pickup"))
	}
	// The delivery can't have to end before the pickup may start.
	if b.Pickup.Earliest != "" && b.Delivery.Latest != "" {
		from, err1 := time.Parse(envelope.TimeLayout, b.Pickup.Earliest)
		to, err2 := time.Parse(envelope.TimeLayout, b.Delivery.Latest)
		if err1 == nil && err2 == nil && to.Before(from) {
			errs = append(errs, errors.New("delivery.latest is before pickup.earliest"))
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

// Validate implements Body.
func (b *RoadBookingConfirmationBody) Validate() error {
	errs := []error{ref("carrier_booking_reference", b.BookingRef)}
	switch b.Status {
	case StatusConfirmed:
		if b.PlannedPickup == "" || b.PlannedDelivery == "" {
			errs = append(errs, errors.New("a confirmation plans the pickup and the delivery"))
		}
		errs = append(errs, window("planned", b.PlannedPickup, b.PlannedDelivery))
	case StatusRejected:
		if b.PlannedPickup != "" || b.PlannedDelivery != "" || len(b.FiscalDocuments) > 0 || len(b.TransportDocuments) > 0 || b.SubscriptionID != "" {
			errs = append(errs, errors.New("a rejection carries no plan, documents or subscription"))
		}
	default:
		errs = append(errs, fmt.Errorf("status: %s is not %s or %s", logsafe.Quote(b.Status), StatusConfirmed, StatusRejected))
	}
	errs = append(errs,
		documents("fiscal_documents", b.FiscalDocuments, fiscalTypes),
		documents("transport_documents", b.TransportDocuments, transportTypes))
	if b.SubscriptionID != "" {
		errs = append(errs, ref("subscription_id", b.SubscriptionID))
	}
	errs = append(errs, validateMessages(b.Messages))
	return errors.Join(errs...)
}

// Validate implements Body.
func (b *RoadBookingAmendmentBody) Validate() error {
	errs := []error{ref("carrier_booking_reference", b.BookingRef)}
	if b.Pickup == nil && b.Delivery == nil {
		errs = append(errs, errors.New("an amendment changes the pickup, the delivery, or both"))
	}
	for _, fw := range []struct {
		field string
		w     *Window
	}{{"pickup", b.Pickup}, {"delivery", b.Delivery}} {
		field, w := fw.field, fw.w
		if w == nil {
			continue
		}
		if w.Earliest == "" && w.Latest == "" {
			errs = append(errs, fmt.Errorf("%s changes nothing", field))
		}
		errs = append(errs, window(field, w.Earliest, w.Latest))
	}
	return errors.Join(errs...)
}
