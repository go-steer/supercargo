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
	"reflect"
	"slices"
	"testing"

	"github.com/go-steer/supercargo/pkg/intents"
)

func voyage() Booking {
	return Booking{Ref: "OB-BKG-1042", Operator: "ocean-blue.example", Shipper: "cymbal-shoes.example",
		Request: intents.BookingRequestBody{PortOfLoading: "NLRTM", PortOfDischarge: "USEWR"},
		Confirmation: intents.BookingConfirmationBody{PlannedDeparture: "2026-10-07T18:00:00Z", PlannedArrival: "2026-10-13T08:00:00Z",
			EBL: &intents.EBL{VesselName: "Ocean Blue Aurora", VoyageNumber: "42W"}}}
}

func ev(id, typ, code, cls, at, port string) Record {
	b, _ := json.Marshal(intents.TnTEventBody{EventID: id, EventType: typ, EventTypeCode: code, EventClassifierCode: cls,
		EventDateTime: at, UNLocationCode: port, Description: "d"})
	return Record{ID: id, Intent: intents.TnTEvent, Body: b}
}

func al(id, prev, next string) Record {
	b, _ := json.Marshal(intents.TnTAlertBody{AlertID: id, Kind: "delay", Severity: "high", Summary: "s", PreviousETA: prev, NewETA: next,
		CauseEventIDs: []string{"EV-5"}})
	return Record{ID: id, Intent: intents.TnTAlert, Body: b}
}

// TestJourneyAsTheVoyageGoes: the plan alone, then each event moves the
// journey on, and every part names the event behind it.
func TestJourneyAsTheVoyageGoes(t *testing.T) {
	b := voyage()

	// Booked, nothing yet: one planned leg, at the port of loading.
	j := BuildJourney(b, nil)
	if len(j.Legs) != 1 || j.Legs[0].Status != LegPlanned || j.Legs[0].Departure != (Moment{At: "2026-10-07T18:00:00Z", Kind: MomentPlanned}) ||
		j.Legs[0].Arrival.Kind != MomentPlanned || j.Legs[0].Vessel != "Ocean Blue Aurora" || len(j.Calls) != 0 ||
		j.Position.At == nil || j.Position.At.UNLocode != "NLRTM" || j.Position.Estimated || j.Position.Since != "" {
		t.Fatalf("booked: %+v", j)
	}
	if p := j.Legs[0].FromPort; p.Name != "Rotterdam" || p.Lat == nil || p.Lng == nil {
		t.Errorf("Rotterdam without its name or coordinates: %+v", p)
	}

	// Gate-in and load: still at Rotterdam, since the load.
	recs := []Record{
		ev("EV-1", "EQUIPMENT", "GTIN", "ACT", "2026-10-06T16:00:00Z", "NLRTM"),
		ev("EV-2", "EQUIPMENT", "LOAD", "ACT", "2026-10-07T14:00:00Z", "NLRTM"),
	}
	j = BuildJourney(b, recs)
	if j.Position.At == nil || j.Position.At.UNLocode != "NLRTM" || j.Position.Since != "EV-2" || len(j.Calls) != 1 ||
		!slices.Equal(j.Calls[0].Kinds, []string{"gate-in", "load"}) || !slices.Equal(j.Calls[0].Events, []string{"EV-1", "EV-2"}) {
		t.Errorf("loaded: %+v", j)
	}

	// Departed: at sea, estimated; the leg is in progress, from the event.
	recs = append(recs, ev("EV-3", "TRANSPORT", "DEPA", "ACT", "2026-10-07T18:00:00Z", "NLRTM"),
		ev("EV-4", "TRANSPORT", "ARRI", "EST", "2026-10-13T08:00:00Z", "USEWR"))
	j = BuildJourney(b, recs)
	if j.Position.At != nil || j.Position.Between != [2]string{"NLRTM", "USEWR"} || !j.Position.Estimated || j.Position.Since != "EV-3" {
		t.Errorf("at sea: %+v", j.Position)
	}
	leg := j.Legs[0]
	if leg.Status != LegInProgress || leg.Departure != (Moment{At: "2026-10-07T18:00:00Z", Kind: MomentActual, Source: "EV-3"}) ||
		leg.Arrival.Source != "EV-4" || leg.Standard != intents.DialectDCSATnT.Standard || !slices.Equal(leg.Sources, []string{"EV-3", "EV-4"}) {
		t.Errorf("in progress: %+v", leg)
	}

	// The delay: a later estimate supersedes, the leg is delayed, the
	// alert is on it; an unscheduled call at Halifax splits the voyage.
	recs = append(recs, ev("EV-5", "TRANSPORT", "ARRI", "EST", "2026-10-16T08:00:00Z", "USEWR"),
		al("AL-6", "2026-10-13T08:00:00Z", "2026-10-16T08:00:00Z"),
		ev("EV-7", "TRANSPORT", "ARRI", "ACT", "2026-10-11T02:00:00Z", "CAHAL"),
		ev("EV-8", "TRANSPORT", "DEPA", "ACT", "2026-10-12T20:00:00Z", "CAHAL"))
	j = BuildJourney(b, recs)
	if len(j.Legs) != 2 || j.Legs[0].ToPort.UNLocode != "CAHAL" || j.Legs[1].FromPort.UNLocode != "CAHAL" {
		t.Fatalf("legs: %+v", j.Legs)
	}
	if j.Legs[0].Status != LegDone || j.Legs[1].Status != LegDelayed || j.Legs[1].Arrival.Source != "EV-5" || j.Legs[1].Departure.Source != "EV-8" {
		t.Errorf("statuses: %s (%+v), %s (%+v)", j.Legs[0].Status, j.Legs[0].Arrival, j.Legs[1].Status, j.Legs[1].Arrival)
	}
	// Newark has only estimates so far: a call to come, with no kinds yet.
	if len(j.Calls) != 3 || j.Calls[1].Port.UNLocode != "CAHAL" || !slices.Contains(j.Calls[1].Kinds, "unscheduled") ||
		len(j.Calls[2].Kinds) != 0 || !slices.Equal(j.Calls[2].Events, []string{"EV-4", "EV-5"}) {
		t.Errorf("calls: %+v", j.Calls)
	}
	if j.Position.Between != [2]string{"CAHAL", "USEWR"} || j.Position.Since != "EV-8" {
		t.Errorf("after Halifax: %+v", j.Position)
	}
	if len(j.Alerts) != 1 || !reflect.DeepEqual(j.Alerts[0], AlertRef{ID: "AL-6", Kind: "delay", Severity: "high", PreviousETA: "2026-10-13T08:00:00Z",
		NewETA: "2026-10-16T08:00:00Z", Leg: 1, Causes: []string{"EV-5"}}) {
		t.Errorf("alerts: %+v", j.Alerts)
	}

	// Arrived: done, at Newark.
	recs = append(recs, ev("EV-9", "TRANSPORT", "ARRI", "ACT", "2026-10-16T09:00:00Z", "USEWR"))
	j = BuildJourney(b, recs)
	if j.Legs[1].Status != LegDone || j.Legs[1].Arrival.Kind != MomentActual || j.Position.At == nil || j.Position.At.UNLocode != "USEWR" {
		t.Errorf("arrived: %+v, %+v", j.Legs[1], j.Position)
	}
}

// TestJourneyEdges: a port this node doesn't know has no coordinates;
// broken records are skipped; a departure from the last port leaves the
// shipment there; planned events count below estimates; calls off the plan
// at the same time sort by code.
func TestJourneyEdges(t *testing.T) {
	b := voyage()
	b.Request.PortOfDischarge = "ZZXXX"
	j := BuildJourney(b, []Record{
		{Intent: intents.TnTEvent, Body: json.RawMessage(`{`)},
		{Intent: intents.TnTAlert, Body: json.RawMessage(`[`)},
		{Intent: "ulp.other", Body: json.RawMessage(`{}`)},
		ev("EV-1", "TRANSPORT", "ARRI", "PLN", "2026-10-13T08:00:00Z", "ZZXXX"),
		ev("EV-2", "TRANSPORT", "ARRI", "EST", "2026-10-14T08:00:00Z", "ZZXXX"),
		ev("EV-3", "TRANSPORT", "ARRI", "PLN", "2026-10-15T08:00:00Z", "ZZXXX"),
		ev("EV-4", "TRANSPORT", "DEPA", "ACT", "2026-10-20T08:00:00Z", "ZZXXX"),
		ev("EV-6", "EQUIPMENT", "LOAD", "ACT", "2026-10-09T08:00:00Z", "DEHAM"),
		ev("EV-5", "EQUIPMENT", "LOAD", "ACT", "2026-10-09T08:00:00Z", "BEANR"),
		ev("EV-7", "EQUIPMENT", "STUF", "ACT", "2026-10-09T08:00:00Z", "BEANR"),
	})
	last := j.Legs[len(j.Legs)-1]
	if last.ToPort.Lat != nil || last.ToPort.Name != "ZZXXX" || last.Arrival.Source != "EV-2" {
		t.Errorf("an unknown port: %+v", last)
	}
	if j.Position.At == nil || j.Position.At.UNLocode != "ZZXXX" || j.Position.Estimated {
		t.Errorf("a departure from the last port: %+v", j.Position)
	}
	// Two calls off the plan at the same time: by code.
	if len(j.Legs) != 3 || j.Legs[0].ToPort.UNLocode != "BEANR" || j.Legs[1].ToPort.UNLocode != "DEHAM" {
		t.Errorf("calls off the plan: %+v", j.Legs)
	}
	if c := j.Calls[0]; c.Port.UNLocode != "BEANR" || !slices.Equal(c.Kinds, []string{"unscheduled", "load"}) || !slices.Equal(c.Events, []string{"EV-5", "EV-7"}) {
		t.Errorf("an event with no call kind: %+v", c)
	}
}
