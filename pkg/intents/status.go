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
	"regexp"

	"github.com/go-steer/supercargo/pkg/internal/logsafe"
)

// DCSA shipment statuses a status response may report.
const (
	StatusBooked        = "BOOKED"
	StatusReceived      = "RECEIVED"
	StatusLoaded        = "LOADED"
	StatusInTransit     = "IN_TRANSIT"
	StatusTransshipment = "TRANSSHIPMENT"
	StatusDischarged    = "DISCHARGED"
	StatusDelivered     = "DELIVERED"
	StatusNotFound      = "NOT_FOUND"
)

var dcsaStatuses = set(StatusBooked, StatusReceived, StatusLoaded, StatusInTransit, StatusTransshipment,
	StatusDischarged, StatusDelivered, StatusNotFound)

// MaxStatusEvents bounds the events a status response carries.
const MaxStatusEvents = 100

// statusRefPattern is pf's pattern for the request's references.
var statusRefPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// TnTStatusRequestBody is the body of ulp.ocean.tnt.request: the DCSA
// Track & Trace status query, as the ULP payload specification wraps it.
type TnTStatusRequestBody struct {
	TnTRequest TnTRequest `json:"tnt_request"`
}

// TnTRequest names the shipment asked about.
type TnTRequest struct {
	TransportDocumentReference string `json:"transport_document_reference"`
	EquipmentReference         string `json:"equipment_reference,omitempty"`
	CarrierBookingReference    string `json:"carrier_booking_reference,omitempty"`
}

// Intent implements Body.
func (*TnTStatusRequestBody) Intent() string { return OceanTnTRequest }

// Validate implements Body.
func (b *TnTStatusRequestBody) Validate() error {
	r := b.TnTRequest
	errs := []error{statusRef("tnt_request.transport_document_reference", r.TransportDocumentReference, 64, true),
		statusRef("tnt_request.equipment_reference", r.EquipmentReference, 32, false),
		statusRef("tnt_request.carrier_booking_reference", r.CarrierBookingReference, 64, false)}
	return errors.Join(errs...)
}

func statusRef(field, v string, max int, required bool) error {
	switch {
	case v == "" && !required:
		return nil
	case len(v) > max || !statusRefPattern.MatchString(v):
		return fmt.Errorf("%s: %s is not 1-%d characters of [A-Za-z0-9_-]", field, logsafe.Quote(v), max)
	}
	return nil
}

// TnTStatusResponseBody is the body of ulp.ocean.tnt.response: the
// shipment's status and its events.
type TnTStatusResponseBody struct {
	TnTResponse TnTStatus `json:"tnt_response"`
}

// TnTStatus is a shipment's status, as DCSA Track & Trace reports it.
type TnTStatus struct {
	TransportDocumentReference string           `json:"transport_document_reference"`
	CarrierBookingReference    string           `json:"carrier_booking_reference,omitempty"`
	EquipmentReference         string           `json:"equipment_reference,omitempty"`
	DCSAStatus                 string           `json:"dcsa_status"`
	VesselName                 string           `json:"vessel_name,omitempty"`
	VesselIMONumber            string           `json:"vessel_imo_number,omitempty"`
	VoyageNumber               string           `json:"voyage_number,omitempty"`
	PortOfLoading              string           `json:"port_of_loading,omitempty"`
	PortOfDischarge            string           `json:"port_of_discharge,omitempty"`
	CurrentLocation            *CurrentLocation `json:"current_location,omitempty"`
	EstimatedTimeOfArrival     string           `json:"estimated_time_of_arrival,omitempty"`
	Events                     []TnTEventBody   `json:"events"`
}

// CurrentLocation is where the shipment last was: the UN/LOCODE, and
// optionally the facility (its code and name) and coordinates, the members
// the ULP payload specification's own status responses fill. Coordinates
// are informative: nothing in ULP decides on them.
type CurrentLocation struct {
	UNLocationCode string   `json:"un_location_code"`
	FacilityCode   string   `json:"facility_code,omitempty"`
	FacilityName   string   `json:"facility_name,omitempty"`
	Lat            *float64 `json:"lat,omitempty"`
	Lng            *float64 `json:"lng,omitempty"`
}

// facilityCodePattern: a terminal or depot code (an SMDG code, a BIC
// facility code), short and inert.
var facilityCodePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$`)

var imoPattern = regexp.MustCompile(`^[0-9]{7}$`)

// Intent implements Body.
func (*TnTStatusResponseBody) Intent() string { return OceanTnTResponse }

// Validate implements Body.
func (b *TnTStatusResponseBody) Validate() error {
	r := b.TnTResponse
	errs := []error{statusRef("tnt_response.transport_document_reference", r.TransportDocumentReference, 64, true),
		statusRef("tnt_response.carrier_booking_reference", r.CarrierBookingReference, 64, false),
		statusRef("tnt_response.equipment_reference", r.EquipmentReference, 32, false)}
	if !dcsaStatuses[r.DCSAStatus] {
		errs = append(errs, fmt.Errorf("tnt_response.dcsa_status: %s is not a DCSA shipment status", logsafe.Quote(r.DCSAStatus)))
	}
	if r.VesselName != "" {
		errs = append(errs, text("tnt_response.vessel_name", r.VesselName, 128, false))
	}
	if r.VesselIMONumber != "" {
		errs = append(errs, match("tnt_response.vessel_imo_number", r.VesselIMONumber, imoPattern))
	}
	if r.VoyageNumber != "" {
		errs = append(errs, ref("tnt_response.voyage_number", r.VoyageNumber))
	}
	for field, v := range map[string]string{"tnt_response.port_of_loading": r.PortOfLoading, "tnt_response.port_of_discharge": r.PortOfDischarge} {
		if v != "" {
			errs = append(errs, match(field, v, unLocodePattern))
		}
	}
	if c := r.CurrentLocation; c != nil {
		errs = append(errs, match("tnt_response.current_location.un_location_code", c.UNLocationCode, unLocodePattern))
		if c.FacilityName != "" {
			errs = append(errs, text("tnt_response.current_location.facility_name", c.FacilityName, 128, false))
		}
		if c.FacilityCode != "" {
			errs = append(errs, match("tnt_response.current_location.facility_code", c.FacilityCode, facilityCodePattern))
		}
		// Both or neither: half a position is no position. NaN fails
		// every comparison, so it is caught too.
		switch {
		case (c.Lat == nil) != (c.Lng == nil):
			errs = append(errs, errors.New("tnt_response.current_location: lat and lng go together"))
		case c.Lat != nil && (!(*c.Lat >= -90 && *c.Lat <= 90) || !(*c.Lng >= -180 && *c.Lng <= 180)):
			errs = append(errs, fmt.Errorf("tnt_response.current_location: %v, %v is not a latitude and longitude", *c.Lat, *c.Lng))
		}
	}
	if r.EstimatedTimeOfArrival != "" {
		_, err := timestamp("tnt_response.estimated_time_of_arrival", r.EstimatedTimeOfArrival)
		errs = append(errs, err)
	}
	if r.DCSAStatus == StatusNotFound && (len(r.Events) > 0 || r.VesselName != "" || r.VesselIMONumber != "" || r.VoyageNumber != "" ||
		r.PortOfLoading != "" || r.PortOfDischarge != "" || r.CurrentLocation != nil || r.EstimatedTimeOfArrival != "") {
		errs = append(errs, errors.New("tnt_response: a NOT_FOUND report carries nothing but the references asked about"))
	}
	if r.Events == nil {
		errs = append(errs, errors.New("tnt_response.events is required (an empty list for none)"))
	}
	if len(r.Events) > MaxStatusEvents {
		errs = append(errs, fmt.Errorf("tnt_response.events has %d events, at most %d", len(r.Events), MaxStatusEvents))
	}
	seen := map[string]bool{}
	for i := range r.Events {
		if err := r.Events[i].Validate(); err != nil {
			errs = append(errs, fmt.Errorf("tnt_response.events[%d]: %w", i, err))
		}
		if seen[r.Events[i].EventID] {
			errs = append(errs, fmt.Errorf("tnt_response.events[%d]: event_id %s repeats", i, logsafe.Quote(r.Events[i].EventID)))
		}
		seen[r.Events[i].EventID] = true
	}
	return errors.Join(errs...)
}
