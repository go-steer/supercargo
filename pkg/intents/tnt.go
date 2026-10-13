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

	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/internal/logsafe"
)

// Event types, classifiers and codes (docs/DESIGN.md, section A.5): the
// DCSA Track & Trace event as the ULP payload specification defines it.
const (
	EventTypeShipment  = "SHIPMENT"
	EventTypeTransport = "TRANSPORT"
	EventTypeEquipment = "EQUIPMENT"

	ClassifierActual    = "ACT"
	ClassifierEstimated = "EST"
	ClassifierPlanned   = "PLN"
)

// DialectDCSATnT is the payload schema of a track & trace event and of
// the status request and response: DCSA Track & Trace 2.2.
var DialectDCSATnT = envelope.Dialect{Standard: "DCSA_TNT", Version: "2.2.0", SchemaURL: "https://dcsa.org/standards/track-and-trace"}

// eventCodes are the DCSA event type codes each event type may carry.
var eventCodes = map[string]map[string]bool{
	EventTypeShipment:  set("RECE", "DRFT", "PENA", "PENU", "REJE", "APPR", "ISSU", "SURR", "SUBM", "VOID", "CONF", "REQS", "CMPL", "HOLD", "RELS"),
	EventTypeTransport: set("ARRI", "DEPA"),
	EventTypeEquipment: set("LOAD", "DISC", "GTIN", "GTOT", "STUF", "STRP", "PICK", "DROP", "INSP", "RSEA", "RMVD"),
}

func set(codes ...string) map[string]bool {
	m := map[string]bool{}
	for _, c := range codes {
		m[c] = true
	}
	return m
}

var classifiers = map[string]bool{ClassifierActual: true, ClassifierEstimated: true, ClassifierPlanned: true}

// TnTEventBody is the body of ulp.tnt.event, a shipment milestone the
// operator pushes to subscribers: the DCSA Track & Trace event, with the
// ULP payload specification's member names.
type TnTEventBody struct {
	EventID             string `json:"event_id"`
	EventType           string `json:"event_type"`
	EventClassifierCode string `json:"event_classifier_code"`
	EventDateTime       string `json:"event_date_time"`
	EventTypeCode       string `json:"event_type_code"`
	Description         string `json:"description"`
	UNLocationCode      string `json:"un_location_code"`
	FacilityName        string `json:"facility_name,omitempty"`
	VesselName          string `json:"vessel_name,omitempty"`
	VoyageNumber        string `json:"voyage_number,omitempty"`
	EquipmentReference  string `json:"equipment_reference,omitempty"`
	// DerivedFrom, a ULP extension, says where a forwarder's event came
	// from: the leg carriers' events it re-issues (DESIGN §A.5).
	DerivedFrom []Provenance `json:"derived_from,omitempty"`
}

// Provenance is one source of a re-issued event or alert: a carrier's
// signed ULP event or alert (by its ID), or an unsigned source the
// re-issuer vouches for (an EDI message, by its reference), so the
// receiver can tell evidence from hearsay and ask for it.
type Provenance struct {
	ID        string `json:"id,omitempty"`        // the source event's or alert's ID
	Source    string `json:"source,omitempty"`    // what it was, e.g. "EDIFACT IFTSTA from polder-haulage.example"
	Reference string `json:"reference,omitempty"` // the source message's own reference
	Signed    bool   `json:"signed"`              // a signed ULP message the re-issuer holds
}

// validateProvenance checks a derived_from list: at most MaxCitations; a
// signed source names its ID; an unsigned one says what it was.
func validateProvenance(ps []Provenance) error {
	if len(ps) > MaxCitations {
		return fmt.Errorf("derived_from has %d sources, over %d", len(ps), MaxCitations)
	}
	var errs []error
	for i, p := range ps {
		at := fmt.Sprintf("derived_from[%d]", i)
		if p.ID != "" {
			errs = append(errs, ref(at+".id", p.ID))
		}
		if p.Source != "" {
			errs = append(errs, text(at+".source", p.Source, 128, false))
		}
		if p.Reference != "" {
			errs = append(errs, match(at+".reference", p.Reference, docNumberPattern))
		}
		switch {
		case p.Signed && p.ID == "":
			errs = append(errs, fmt.Errorf("%s: a signed source names the event or alert it was (id)", at))
		case !p.Signed && p.Source == "":
			errs = append(errs, fmt.Errorf("%s: an unsigned source says what it was (source)", at))
		}
	}
	return errors.Join(errs...)
}

// Intent implements Body.
func (*TnTEventBody) Intent() string { return TnTEvent }

// Validate implements Body. An equipment event names its container.
func (b *TnTEventBody) Validate() error {
	errs := []error{ref("event_id", b.EventID), text("description", b.Description, 280, false),
		match("un_location_code", b.UNLocationCode, unLocodePattern), validateProvenance(b.DerivedFrom)}
	codes, ok := eventCodes[b.EventType]
	switch {
	case !ok:
		errs = append(errs, fmt.Errorf("event_type: %s is not SHIPMENT, TRANSPORT or EQUIPMENT", logsafe.Quote(b.EventType)))
	case !codes[b.EventTypeCode]:
		errs = append(errs, fmt.Errorf("event_type_code: %s is not a %s event code", logsafe.Quote(b.EventTypeCode), b.EventType))
	}
	if !classifiers[b.EventClassifierCode] {
		errs = append(errs, fmt.Errorf("event_classifier_code: %s is not ACT, EST or PLN", logsafe.Quote(b.EventClassifierCode)))
	}
	if _, err := timestamp("event_date_time", b.EventDateTime); err != nil {
		errs = append(errs, err)
	}
	if b.EventType == EventTypeEquipment && b.EquipmentReference == "" {
		errs = append(errs, errors.New("equipment_reference is required for an EQUIPMENT event"))
	}
	if b.EquipmentReference != "" {
		errs = append(errs, match("equipment_reference", b.EquipmentReference, containerPattern))
	}
	if b.FacilityName != "" {
		errs = append(errs, text("facility_name", b.FacilityName, 128, false))
	}
	if b.VesselName != "" {
		errs = append(errs, text("vessel_name", b.VesselName, 128, false))
	}
	if b.VoyageNumber != "" {
		errs = append(errs, ref("voyage_number", b.VoyageNumber))
	}
	return errors.Join(errs...)
}

// Alert kinds and severities.
const (
	AlertDelay     = "delay"
	AlertEarly     = "early"
	AlertException = "exception"

	SeverityLow    = "low"
	SeverityMedium = "medium"
	SeverityHigh   = "high"
)

var (
	alertKinds = map[string]bool{AlertDelay: true, AlertEarly: true, AlertException: true}
	severities = map[string]bool{SeverityLow: true, SeverityMedium: true, SeverityHigh: true}
)

// MaxCauseEvents bounds the events an alert may cite.
const MaxCauseEvents = MaxCitations

// TnTAlertBody is the body of ulp.tnt.alert: the operator telling
// subscribers, unasked, that something changed.
type TnTAlertBody struct {
	AlertID       string   `json:"alert_id"`
	Kind          string   `json:"kind"`
	Severity      string   `json:"severity"`
	Summary       string   `json:"summary"`
	PreviousETA   string   `json:"previous_eta,omitempty"`
	NewETA        string   `json:"new_eta,omitempty"`
	CauseEventIDs []string `json:"cause_event_ids"`
	// DerivedFrom says where a forwarder's alert came from (a leg
	// carrier's alert), as on events.
	DerivedFrom []Provenance `json:"derived_from,omitempty"`
}

// Intent implements Body.
func (*TnTAlertBody) Intent() string { return TnTAlert }

// Validate implements Body. A delay or early alert carries both ETAs, in
// the order its kind says.
func (b *TnTAlertBody) Validate() error {
	errs := []error{ref("alert_id", b.AlertID), text("summary", b.Summary, 280, false), validateProvenance(b.DerivedFrom)}
	if !alertKinds[b.Kind] {
		errs = append(errs, fmt.Errorf("kind: %s is not delay, early or exception", logsafe.Quote(b.Kind)))
	}
	if !severities[b.Severity] {
		errs = append(errs, fmt.Errorf("severity: %s is not low, medium or high", logsafe.Quote(b.Severity)))
	}
	if b.Kind == AlertDelay || b.Kind == AlertEarly {
		prev, err1 := timestamp("previous_eta", b.PreviousETA)
		next, err2 := timestamp("new_eta", b.NewETA)
		errs = append(errs, err1, err2)
		if err1 == nil && err2 == nil {
			if b.Kind == AlertDelay && !next.After(prev) {
				errs = append(errs, errors.New("a delay's new_eta is not after its previous_eta"))
			}
			if b.Kind == AlertEarly && !next.Before(prev) {
				errs = append(errs, errors.New("an early alert's new_eta is not before its previous_eta"))
			}
		}
	} else {
		for field, v := range map[string]string{"previous_eta": b.PreviousETA, "new_eta": b.NewETA} {
			if v != "" {
				_, err := timestamp(field, v)
				errs = append(errs, err)
			}
		}
	}
	errs = append(errs, citations("cause_event_ids", b.CauseEventIDs))
	return errors.Join(errs...)
}
