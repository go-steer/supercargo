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

package store

import (
	"encoding/json"
	"testing"

	"github.com/go-steer/supercargo/pkg/intents"
)

// TestOrderJourney: a door-to-door order's journey has the plan's legs,
// each with its mode and carrier; events replace the plan's times; the
// order's latest delay moves the delivery; an event off the plan's places
// doesn't redraw the route.
func TestOrderJourney(t *testing.T) {
	b := orderBooking()
	b.Order.Confirmation.PlannedDelivery = "2026-10-27T02:00:00Z"
	b.Order.Confirmation.Legs = []intents.PlannedLeg{
		{Mode: intents.ModeRoad, From: "NLTLB", To: "NLRTM", Carrier: "Polder Haulage", PlannedDeparture: "2026-10-05T06:00:00Z", PlannedArrival: "2026-10-05T09:00:00Z"},
		{Mode: intents.ModeOcean, From: "NLRTM", To: "BRSSZ", Carrier: "Ocean Blue", Vessel: "Ocean Blue Aurora 42W", PlannedDeparture: "2026-10-07T18:00:00Z", PlannedArrival: "2026-10-23T14:00:00Z"},
		{Mode: intents.ModeRoad, From: "BRSSZ", To: "BRSAO", Carrier: "Serra Transportes", PlannedDeparture: "2026-10-25T20:00:00Z"},
	}
	ev := func(id, code, class, at, loc string) Record {
		raw, _ := json.Marshal(intents.TnTEventBody{EventID: id, EventType: intents.EventTypeTransport, EventTypeCode: code, EventClassifierCode: class,
			EventDateTime: at, UNLocationCode: loc})
		return Record{ID: id, Intent: intents.TnTEvent, Body: raw}
	}
	recs := []Record{
		ev("EV-1", "DEPA", intents.ClassifierActual, "2026-10-05T06:10:00Z", "NLTLB"),
		ev("EV-2", "ARRI", intents.ClassifierActual, "2026-10-05T09:20:00Z", "NLRTM"),
		ev("EV-3", "DEPA", intents.ClassifierActual, "2026-10-07T18:00:00Z", "NLRTM"),
		ev("EV-4", "ARRI", intents.ClassifierActual, "2026-10-12T08:00:00Z", "ESLPA"), // an unscheduled call
		ev("EV-5", "ARRI", intents.ClassifierEstimated, "2026-10-26T14:00:00Z", "BRSSZ"),
	}
	j := BuildJourney(b, recs)
	if len(j.Legs) != 3 {
		t.Fatalf("legs %+v", j.Legs)
	}
	l0, l1, l2 := j.Legs[0], j.Legs[1], j.Legs[2]
	if l0.Mode != intents.ModeRoad || l0.Carrier != "Polder Haulage" || l0.Status != LegDone || l0.Departure.Source != "EV-1" || l0.Arrival.Source != "EV-2" {
		t.Errorf("leg 1 %+v", l0)
	}
	if l1.Mode != intents.ModeOcean || l1.Vessel != "Ocean Blue Aurora 42W" || l1.Status != LegDelayed || l1.Arrival.At != "2026-10-26T14:00:00Z" ||
		l1.PlannedArrival != "2026-10-23T14:00:00Z" {
		t.Errorf("leg 2 %+v", l1)
	}
	// The last leg ends with the delivery: its plan's, until an alert moves it.
	if l2.Status != LegPlanned || l2.Arrival.At != "2026-10-27T02:00:00Z" || l2.Arrival.Kind != MomentPlanned || l2.PlannedArrival != "" {
		t.Errorf("leg 3 %+v", l2)
	}
	if j.Position.Between != [2]string{"NLRTM", "BRSSZ"} || !j.Position.Estimated {
		t.Errorf("position %+v", j.Position)
	}
	for _, c := range j.Calls {
		if c.Port.UNLocode == "ESLPA" {
			t.Error("an event off the plan redrew the route")
		}
	}
	raw, _ := json.Marshal(intents.TnTAlertBody{AlertID: "AL-1", Kind: intents.AlertDelay, Severity: intents.SeverityHigh, Summary: "Later.",
		PreviousETA: "2026-10-27T02:00:00Z", NewETA: "2026-10-30T02:00:00Z", CauseEventIDs: []string{"EV-5", "EV-elsewhere"}})
	recs = append(recs, Record{ID: "AL-1", Intent: intents.TnTAlert, Body: raw})
	j = BuildJourney(b, recs)
	l2 = j.Legs[2]
	if l2.Status != LegDelayed || l2.Arrival.At != "2026-10-30T02:00:00Z" || l2.Arrival.Source != "AL-1" || l2.PlannedArrival != "2026-10-27T02:00:00Z" {
		t.Errorf("leg 3 after the delay %+v", l2)
	}
	if len(j.Alerts) != 1 || j.Alerts[0].Leg != 2 || len(j.Alerts[0].Causes) != 1 || j.Alerts[0].Causes[0] != "EV-5" {
		t.Errorf("alerts %+v", j.Alerts)
	}
	// The summary agrees: the delivery is where the alert moved it.
	if s := Summarize(b, recs); s.Kind != KindOrder || s.ETA != "2026-10-30T02:00:00Z" || s.ETASource != "AL-1" || s.PlannedArrival != "2026-10-27T02:00:00Z" {
		t.Errorf("summary %+v", s)
	}
	// Delivered: the actual arrival stands, whatever the alerts said.
	recs = append(recs, ev("EV-9", "ARRI", intents.ClassifierActual, "2026-10-29T10:00:00Z", "BRSAO"))
	if l := BuildJourney(b, recs).Legs[2]; l.Status != LegDone || l.Arrival.Source != "EV-9" {
		t.Errorf("delivered %+v", l)
	}
	if s := Summarize(b, recs); s.ETA != "2026-10-29T10:00:00Z" {
		t.Errorf("delivered summary %+v", s)
	}
}

// TestOrderJourneyRoundTrip: a plan that comes back to a place it left
// lists the place's events once; a leg under way with no estimate is in
// progress.
func TestOrderJourneyRoundTrip(t *testing.T) {
	b := orderBooking()
	b.Order.Confirmation.Legs = []intents.PlannedLeg{
		{Mode: intents.ModeRoad, From: "NLTLB", To: "NLRTM"},
		{Mode: intents.ModeRoad, From: "NLRTM", To: "NLTLB"},
	}
	raw, _ := json.Marshal(intents.TnTEventBody{EventID: "EV-1", EventType: intents.EventTypeTransport, EventTypeCode: "DEPA",
		EventClassifierCode: intents.ClassifierActual, EventDateTime: "2026-10-05T06:10:00Z", UNLocationCode: "NLTLB"})
	j := BuildJourney(b, []Record{{ID: "EV-1", Intent: intents.TnTEvent, Body: raw}})
	if len(j.Calls) != 1 || len(j.Calls[0].Events) != 1 {
		t.Errorf("calls %+v", j.Calls)
	}
	if j.Legs[0].Status != LegInProgress {
		t.Errorf("leg 1 %+v", j.Legs[0])
	}
	// Leg 1's departure is at leg 2's far end, but leg 2 hasn't left: not done.
	if j.Legs[1].Status != LegPlanned {
		t.Errorf("leg 2 %+v", j.Legs[1])
	}
}

// TestOrderJourneyHandover: something at a leg's far end before the leg
// left (an empty container picked up at the port) doesn't make it done.
func TestOrderJourneyHandover(t *testing.T) {
	b := orderBooking()
	b.Order.Confirmation.Legs = []intents.PlannedLeg{
		{Mode: intents.ModeOcean, From: "NLRTM", To: "BRSSZ"},
		{Mode: intents.ModeRoad, From: "BRSSZ", To: "BRSAO"},
	}
	ev := func(id, code, at, loc string) Record {
		raw, _ := json.Marshal(intents.TnTEventBody{EventID: id, EventType: intents.EventTypeEquipment, EventTypeCode: code,
			EventClassifierCode: intents.ClassifierActual, EventDateTime: at, UNLocationCode: loc})
		return Record{ID: id, Intent: intents.TnTEvent, Body: raw}
	}
	recs := []Record{ev("EV-1", "GTOT", "2026-10-01T08:00:00Z", "BRSSZ"), ev("EV-2", "DEPA", "2026-10-07T18:00:00Z", "NLRTM")}
	if l := BuildJourney(b, recs).Legs[0]; l.Status != LegInProgress {
		t.Errorf("before: %+v", l)
	}
	recs = append(recs, ev("EV-3", "DISC", "2026-10-23T16:00:00Z", "BRSSZ"))
	if l := BuildJourney(b, recs).Legs[0]; l.Status != LegDone {
		t.Errorf("discharged: %+v", l)
	}
}

// TestOrderETAPrecedence: the order's latest alert moves the delivery over
// an estimate that came after it, in the summary as in the journey.
func TestOrderETAPrecedence(t *testing.T) {
	b := orderBooking()
	alert, _ := json.Marshal(intents.TnTAlertBody{AlertID: "AL-1", Kind: intents.AlertDelay, Severity: intents.SeverityHigh, Summary: "Later.",
		PreviousETA: "2026-10-26T10:00:00Z", NewETA: "2026-10-30T02:00:00Z"})
	est, _ := json.Marshal(intents.TnTEventBody{EventID: "EV-1", EventType: intents.EventTypeTransport, EventTypeCode: "ARRI",
		EventClassifierCode: intents.ClassifierEstimated, EventDateTime: "2026-10-29T02:00:00Z", UNLocationCode: "BRSAO"})
	recs := []Record{{ID: "AL-1", Intent: intents.TnTAlert, Body: alert}, {ID: "EV-1", Intent: intents.TnTEvent, Body: est}}
	s, j := Summarize(b, recs), BuildJourney(b, recs)
	if s.ETA != "2026-10-30T02:00:00Z" || s.ETASource != "AL-1" || j.Legs[0].Arrival.At != s.ETA {
		t.Errorf("summary %s (%s), journey %s", s.ETA, s.ETASource, j.Legs[0].Arrival.At)
	}
}

// TestSummaryKinds: a summary says what was booked.
func TestSummaryKinds(t *testing.T) {
	if s := Summarize(orderBooking(), nil); s.Kind != KindOrder {
		t.Errorf("order: %q", s.Kind)
	}
	if s := Summarize(Booking{Ref: "OB-1"}, nil); s.Kind != KindOcean {
		t.Errorf("ocean: %q", s.Kind)
	}
	if s := Summarize(Booking{Ref: "SR-1", Road: &RoadBooking{}}, nil); s.Kind != KindRoad {
		t.Errorf("road: %q", s.Kind)
	}
}
