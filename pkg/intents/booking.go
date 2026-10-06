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

// BookingRequestBody is the body of ulp.booking.request. The shipper's
// required delivery date is deliberately not part of it: that is private
// context, used only by the shipper's own model.
type BookingRequestBody struct {
	ShipperRef        string        `json:"shipperRef"`
	Origin            Location      `json:"origin"`
	Destination       Location      `json:"destination"`
	Equipment         []Equipment   `json:"equipment"`
	Cargo             Cargo         `json:"cargo"`
	EarliestDeparture string        `json:"earliestDeparture"`
	Consignee         *Consignee    `json:"consignee,omitempty"`
	Subscription      *Subscription `json:"subscription,omitempty"`
}

// Location is a place, identified by its UN/LOCODE.
type Location struct {
	UNLocode string `json:"unLocode"`
	Name     string `json:"name"`
}

// Equipment is a number of containers of one ISO 6346 size-type.
type Equipment struct {
	ISOType string `json:"isoType"`
	Label   string `json:"label"`
	Count   int    `json:"count"`
}

// Cargo describes what is shipped.
type Cargo struct {
	Description string `json:"description"`
	HSCode      string `json:"hsCode"`
}

// Consignee is the receiving party. Domain makes it a ULP party that can be
// granted delegated access later.
type Consignee struct {
	Name   string `json:"name"`
	Domain string `json:"domain"`
}

// Subscription asks the operator to push the listed intents for the new
// booking to Endpoint.
type Subscription struct {
	Endpoint string   `json:"endpoint"`
	Intents  []string `json:"intents"`
}

// Limits on booking request lists.
const (
	MaxEquipmentLines = 20
	MaxEquipmentCount = 999
)

// Intent implements Body.
func (*BookingRequestBody) Intent() string { return BookingRequest }

// Validate implements Body.
func (b *BookingRequestBody) Validate() error {
	errs := []error{
		ref("shipperRef", b.ShipperRef),
		b.Origin.validate("origin"),
		b.Destination.validate("destination"),
		b.Cargo.validate(),
		date("earliestDeparture", b.EarliestDeparture),
	}
	if len(b.Equipment) == 0 || len(b.Equipment) > MaxEquipmentLines {
		errs = append(errs, fmt.Errorf("equipment has %d lines, want 1-%d", len(b.Equipment), MaxEquipmentLines))
	}
	for i, e := range b.Equipment {
		errs = append(errs, e.validate(fmt.Sprintf("equipment[%d]", i)))
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

func (l Location) validate(field string) error {
	return errors.Join(
		match(field+".unLocode", l.UNLocode, unLocodePattern),
		text(field+".name", l.Name, 128, false),
	)
}

func (e Equipment) validate(field string) error {
	errs := []error{
		match(field+".isoType", e.ISOType, isoTypePattern),
		text(field+".label", e.Label, 16, false),
	}
	if e.Count < 1 || e.Count > MaxEquipmentCount {
		errs = append(errs, fmt.Errorf("%s.count is %d, want 1-%d", field, e.Count, MaxEquipmentCount))
	}
	return errors.Join(errs...)
}

func (c Cargo) validate() error {
	return errors.Join(
		text("cargo.description", c.Description, 512, false),
		match("cargo.hsCode", c.HSCode, hsCodePattern),
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

// BookingConfirmationBody is the body of ulp.booking.confirmation, the
// operator's answer to a booking request.
type BookingConfirmationBody struct {
	BookingRef       string    `json:"bookingRef"`
	Status           string    `json:"status"`
	Vessel           *Vessel   `json:"vessel,omitempty"`
	PlannedDeparture string    `json:"plannedDeparture,omitempty"`
	PlannedArrival   string    `json:"plannedArrival,omitempty"`
	EquipmentRefs    []string  `json:"equipmentRefs,omitempty"`
	SubscriptionID   string    `json:"subscriptionId,omitempty"`
	Messages         []Message `json:"messages,omitempty"`
}

// Vessel is the ship and voyage a booking travels on.
type Vessel struct {
	Name   string `json:"name"`
	Voyage string `json:"voyage"`
}

// Message is a coded note attached to a response.
type Message struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// Intent implements Body.
func (*BookingConfirmationBody) Intent() string { return BookingConfirmation }

// Validate implements Body. A confirmed booking names its vessel and its
// planned departure and arrival, in that order.
func (b *BookingConfirmationBody) Validate() error {
	errs := []error{ref("bookingRef", b.BookingRef)}
	switch b.Status {
	case StatusConfirmed:
		if b.Vessel == nil {
			errs = append(errs, errors.New("vessel is required when confirmed"))
		}
		dep, err1 := timestamp("plannedDeparture", b.PlannedDeparture)
		arr, err2 := timestamp("plannedArrival", b.PlannedArrival)
		errs = append(errs, err1, err2)
		if err1 == nil && err2 == nil && !arr.After(dep) {
			errs = append(errs, errors.New("plannedArrival is not after plannedDeparture"))
		}
	case StatusRejected, StatusAlternativeProposed:
		for field, v := range map[string]string{"plannedDeparture": b.PlannedDeparture, "plannedArrival": b.PlannedArrival} {
			if v != "" {
				_, err := timestamp(field, v)
				errs = append(errs, err)
			}
		}
	default:
		errs = append(errs, fmt.Errorf("status: %s is not confirmed, rejected or alternative_proposed", logsafe.Quote(b.Status)))
	}
	if b.Vessel != nil {
		errs = append(errs, text("vessel.name", b.Vessel.Name, 128, false), ref("vessel.voyage", b.Vessel.Voyage))
	}
	if len(b.EquipmentRefs) > MaxEquipmentLines*MaxEquipmentCount {
		errs = append(errs, fmt.Errorf("equipmentRefs has %d entries", len(b.EquipmentRefs)))
	}
	for i, r := range b.EquipmentRefs {
		errs = append(errs, match(fmt.Sprintf("equipmentRefs[%d]", i), r, containerPattern))
	}
	if b.SubscriptionID != "" {
		errs = append(errs, ref("subscriptionId", b.SubscriptionID))
	}
	errs = append(errs, validateMessages(b.Messages))
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
