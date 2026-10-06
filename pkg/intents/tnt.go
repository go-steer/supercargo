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

	"github.com/go-steer/supercargo/pkg/internal/logsafe"
)

// Event types, classifiers and codes (docs/DESIGN.md, section A.5): a
// DCSA-style subset.
const (
	EventTypeEquipment = "EQUIPMENT"
	EventTypeTransport = "TRANSPORT"

	ClassifierActual    = "ACT"
	ClassifierEstimated = "EST"
	ClassifierPlanned   = "PLN"
)

// eventCodes are the codes each event type may carry.
var eventCodes = map[string]map[string]bool{
	EventTypeEquipment: {"GTIN": true, "LOAD": true, "DISC": true, "GTOT": true},
	EventTypeTransport: {"DEPA": true, "ARRI": true},
}

var classifiers = map[string]bool{ClassifierActual: true, ClassifierEstimated: true, ClassifierPlanned: true}

// TnTEventBody is the body of ulp.tnt.event, a shipment milestone the
// operator pushes to subscribers.
type TnTEventBody struct {
	EventID             string     `json:"eventId"`
	EventType           string     `json:"eventType"`
	EventClassifierCode string     `json:"eventClassifierCode"`
	EventCode           string     `json:"eventCode"`
	EventDateTime       string     `json:"eventDateTime"`
	Location            Location   `json:"location"`
	EquipmentRef        string     `json:"equipmentRef,omitempty"`
	Transport           *Transport `json:"transport,omitempty"`
}

// Transport names the vessel and voyage an event concerns.
type Transport struct {
	Vessel string `json:"vessel"`
	Voyage string `json:"voyage"`
}

// Intent implements Body.
func (*TnTEventBody) Intent() string { return TnTEvent }

// Validate implements Body. An equipment event names its container; a
// transport event names its vessel and voyage.
func (b *TnTEventBody) Validate() error {
	errs := []error{ref("eventId", b.EventID), b.Location.validate("location")}
	codes, ok := eventCodes[b.EventType]
	switch {
	case !ok:
		errs = append(errs, fmt.Errorf("eventType: %s is not EQUIPMENT or TRANSPORT", logsafe.Quote(b.EventType)))
	case !codes[b.EventCode]:
		errs = append(errs, fmt.Errorf("eventCode: %s is not a %s event code", logsafe.Quote(b.EventCode), b.EventType))
	}
	if !classifiers[b.EventClassifierCode] {
		errs = append(errs, fmt.Errorf("eventClassifierCode: %s is not ACT, EST or PLN", logsafe.Quote(b.EventClassifierCode)))
	}
	if _, err := timestamp("eventDateTime", b.EventDateTime); err != nil {
		errs = append(errs, err)
	}
	if b.EventType == EventTypeEquipment && b.EquipmentRef == "" {
		errs = append(errs, errors.New("equipmentRef is required for an EQUIPMENT event"))
	}
	if b.EquipmentRef != "" {
		errs = append(errs, match("equipmentRef", b.EquipmentRef, containerPattern))
	}
	if b.EventType == EventTypeTransport && b.Transport == nil {
		errs = append(errs, errors.New("transport is required for a TRANSPORT event"))
	}
	if b.Transport != nil {
		errs = append(errs, text("transport.vessel", b.Transport.Vessel, 128, false), ref("transport.voyage", b.Transport.Voyage))
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
const MaxCauseEvents = 20

// TnTAlertBody is the body of ulp.tnt.alert: the operator telling
// subscribers, unasked, that something changed.
type TnTAlertBody struct {
	AlertID       string   `json:"alertId"`
	Kind          string   `json:"kind"`
	Severity      string   `json:"severity"`
	Summary       string   `json:"summary"`
	PreviousETA   string   `json:"previousEta,omitempty"`
	NewETA        string   `json:"newEta,omitempty"`
	CauseEventIDs []string `json:"causeEventIds"`
}

// Intent implements Body.
func (*TnTAlertBody) Intent() string { return TnTAlert }

// Validate implements Body. A delay or early alert carries both ETAs, in
// the order its kind says.
func (b *TnTAlertBody) Validate() error {
	errs := []error{ref("alertId", b.AlertID), text("summary", b.Summary, 280, false)}
	if !alertKinds[b.Kind] {
		errs = append(errs, fmt.Errorf("kind: %s is not delay, early or exception", logsafe.Quote(b.Kind)))
	}
	if !severities[b.Severity] {
		errs = append(errs, fmt.Errorf("severity: %s is not low, medium or high", logsafe.Quote(b.Severity)))
	}
	if b.Kind == AlertDelay || b.Kind == AlertEarly {
		prev, err1 := timestamp("previousEta", b.PreviousETA)
		next, err2 := timestamp("newEta", b.NewETA)
		errs = append(errs, err1, err2)
		if err1 == nil && err2 == nil {
			if b.Kind == AlertDelay && !next.After(prev) {
				errs = append(errs, errors.New("a delay's newEta is not after its previousEta"))
			}
			if b.Kind == AlertEarly && !next.Before(prev) {
				errs = append(errs, errors.New("an early alert's newEta is not before its previousEta"))
			}
		}
	} else {
		for field, v := range map[string]string{"previousEta": b.PreviousETA, "newEta": b.NewETA} {
			if v != "" {
				_, err := timestamp(field, v)
				errs = append(errs, err)
			}
		}
	}
	if len(b.CauseEventIDs) > MaxCauseEvents {
		errs = append(errs, fmt.Errorf("causeEventIds has %d entries, over %d", len(b.CauseEventIDs), MaxCauseEvents))
	}
	seen := map[string]bool{}
	for i, id := range b.CauseEventIDs {
		errs = append(errs, ref(fmt.Sprintf("causeEventIds[%d]", i), id))
		if seen[id] {
			errs = append(errs, fmt.Errorf("causeEventIds[%d] repeats %s", i, logsafe.Quote(id)))
		}
		seen[id] = true
	}
	return errors.Join(errs...)
}
