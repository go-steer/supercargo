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
	"cmp"
	"encoding/json"
	"slices"
	"strings"

	"github.com/go-steer/supercargo/internal/ports"
	"github.com/go-steer/supercargo/pkg/intents"
)

// Journey is a booking's journey as this node's records show it
// (docs/ui/route-map.md): its legs, the ports with events, where the
// shipment is, and the alerts. Every part names the records it came from,
// so a view can link each mark to the signed message behind it; nothing
// in it is hardcoded or guessed beyond what is marked estimated.
type Journey struct {
	Booking  string     `json:"booking"`
	Operator string     `json:"operator"`
	Shipper  string     `json:"shipper"`
	Legs     []Leg      `json:"legs"`
	Calls    []Call     `json:"calls"`
	Position Position   `json:"position"`
	Alerts   []AlertRef `json:"alerts"`
}

// Point is a port: its UN/LOCODE, and the name and coordinates this node
// knows for it (none for a port it doesn't know).
type Point struct {
	UNLocode string   `json:"unLocode"`
	Name     string   `json:"name"`
	Lat      *float64 `json:"lat,omitempty"`
	Lng      *float64 `json:"lng,omitempty"`
}

func point(code string) Point {
	p := Point{UNLocode: code, Name: ports.Name(code)}
	if lat, lng, ok := ports.Coords(code); ok {
		p.Lat, p.Lng = &lat, &lng
	}
	return p
}

// Moment is a time, how firm it is, and the event it came from (none for
// the booking's plan).
type Moment struct {
	At     string `json:"at,omitempty"`
	Kind   string `json:"kind,omitempty"` // MomentPlanned, MomentEstimated or MomentActual
	Source string `json:"source,omitempty"`
}

// How firm a moment is.
const (
	MomentPlanned   = "planned"
	MomentEstimated = "estimated"
	MomentActual    = "actual"
)

// Leg statuses.
const (
	LegPlanned    = "planned"
	LegInProgress = "in progress"
	LegDone       = "done"
	LegDelayed    = "delayed"
)

// Leg is one hop between two ports.
type Leg struct {
	FromPort  Point  `json:"from"`
	ToPort    Point  `json:"to"`
	Mode      string `json:"mode"` // "ocean" (road with the truck leg, #80)
	Vessel    string `json:"vessel,omitempty"`
	Voyage    string `json:"voyage,omitempty"`
	Departure Moment `json:"departure"`
	Arrival   Moment `json:"arrival"`
	Status    string `json:"status"`
	// Standard is the dialect of the messages that report the leg
	// ("DCSA_TNT"), for a view to name it.
	Standard string   `json:"standard,omitempty"`
	Sources  []string `json:"sources"`
}

// Call is a port with events: what happened there, and the events.
type Call struct {
	Port   Point    `json:"port"`
	Kinds  []string `json:"kinds"` // gate-in, load, depart, arrive, discharge, gate-out; unscheduled for a port off the plan
	Events []string `json:"events"`
}

// Position is where the shipment is: at a port (since an actual event
// there), or between two ports after an actual departure, which is an
// estimate: no message says where a vessel is at sea.
type Position struct {
	At        *Point    `json:"at,omitempty"`
	Between   [2]string `json:"between,omitzero"`
	Estimated bool      `json:"estimated"`
	Since     string    `json:"since,omitempty"` // the event it follows from; none before any
}

// AlertRef is an alert on the journey: the leg it concerns (the arrival's)
// and what it says.
type AlertRef struct {
	ID          string   `json:"id"`
	Kind        string   `json:"kind"`
	Severity    string   `json:"severity"`
	PreviousETA string   `json:"previousEta,omitempty"`
	NewETA      string   `json:"newEta,omitempty"`
	Leg         int      `json:"leg"`
	Causes      []string `json:"causes,omitempty"`
}

var callKinds = map[string]string{"GTIN": "gate-in", "LOAD": "load", "DEPA": "depart", "ARRI": "arrive", "DISC": "discharge", "GTOT": "gate-out"}

// BuildJourney derives b's journey from its records (as Summarize does
// its summary): the plan comes from the booking, everything else from the
// events and alerts held.
func BuildJourney(b Booking, recs []Record) Journey {
	c := b.Confirmation
	pol, pod := b.Request.PortOfLoading, b.Request.PortOfDischarge
	j := Journey{Booking: b.Ref, Operator: b.Operator, Shipper: b.Shipper, Legs: []Leg{}, Calls: []Call{}, Alerts: []AlertRef{}}

	var events []intents.TnTEventBody
	var alerts []intents.TnTAlertBody
	for _, r := range recs {
		switch r.Intent {
		case intents.TnTEvent:
			var e intents.TnTEventBody
			if json.Unmarshal(r.Body, &e) == nil { // the node's own, validated when stored
				events = append(events, e)
			}
		case intents.TnTAlert:
			var a intents.TnTAlertBody
			if json.Unmarshal(r.Body, &a) == nil {
				alerts = append(alerts, a)
			}
		}
	}

	// The ports, in order: loading, any calls off the plan (in the order
	// their events happened), discharge.
	route := []string{pol}
	extra := map[string]string{} // port → its earliest event time
	for _, e := range events {
		if p := e.UNLocationCode; p != pol && p != pod {
			if t, ok := extra[p]; !ok || e.EventDateTime < t {
				extra[p] = e.EventDateTime
			}
		}
	}
	mid := make([]string, 0, len(extra))
	for p := range extra {
		mid = append(mid, p)
	}
	// By when each was first reported, then by code.
	slices.SortFunc(mid, func(a, b string) int { return cmp.Or(strings.Compare(extra[a], extra[b]), strings.Compare(a, b)) })
	route = append(append(route, mid...), pod)

	for _, p := range route {
		call := Call{Port: point(p), Kinds: []string{}, Events: []string{}}
		if extra[p] != "" {
			call.Kinds = append(call.Kinds, "unscheduled")
		}
		for _, e := range events {
			if e.UNLocationCode == p {
				if k, ok := callKinds[e.EventTypeCode]; ok && e.EventClassifierCode == intents.ClassifierActual && !slices.Contains(call.Kinds, k) {
					call.Kinds = append(call.Kinds, k)
				}
				call.Events = append(call.Events, e.EventID)
			}
		}
		if len(call.Events) > 0 {
			j.Calls = append(j.Calls, call)
		}
	}

	vessel, voyage := c.Vessel()
	for i := 0; i+1 < len(route); i++ {
		from, to := route[i], route[i+1]
		leg := Leg{FromPort: point(from), ToPort: point(to), Mode: "ocean", Vessel: vessel, Voyage: voyage, Sources: []string{}}
		leg.Departure = moment(events, from, "DEPA")
		if leg.Departure.At == "" && i == 0 {
			leg.Departure = Moment{At: c.PlannedDeparture, Kind: MomentPlanned}
		}
		leg.Arrival = moment(events, to, "ARRI")
		if leg.Arrival.At == "" && i == len(route)-2 {
			leg.Arrival = Moment{At: c.PlannedArrival, Kind: MomentPlanned}
		}
		for _, m := range []Moment{leg.Departure, leg.Arrival} {
			if m.Source != "" {
				leg.Sources = append(leg.Sources, m.Source)
				leg.Standard = intents.DialectDCSATnT.Standard
			}
		}
		switch {
		case leg.Arrival.Kind == MomentActual:
			leg.Status = LegDone
		case i == len(route)-2 && leg.Arrival.Kind == MomentEstimated && leg.Arrival.At > c.PlannedArrival:
			leg.Status = LegDelayed
		case leg.Departure.Kind == MomentActual:
			leg.Status = LegInProgress
		default:
			leg.Status = LegPlanned
		}
		j.Legs = append(j.Legs, leg)
	}

	j.Position = position(events, route, pol)
	for _, a := range alerts {
		j.Alerts = append(j.Alerts, AlertRef{ID: a.AlertID, Kind: a.Kind, Severity: a.Severity, PreviousETA: a.PreviousETA,
			NewETA: a.NewETA, Leg: len(j.Legs) - 1, Causes: a.CauseEventIDs})
	}
	return j
}

// moment is the firmest, latest time of a code (DEPA, ARRI) at port: an
// actual one if any, else the latest estimate (a later one supersedes),
// else the latest plan.
func moment(events []intents.TnTEventBody, port, code string) Moment {
	var best Moment
	rank := map[string]int{intents.ClassifierPlanned: 1, intents.ClassifierEstimated: 2, intents.ClassifierActual: 3}
	kinds := map[string]string{intents.ClassifierPlanned: MomentPlanned, intents.ClassifierEstimated: MomentEstimated, intents.ClassifierActual: MomentActual}
	bestRank := 0
	for _, e := range events {
		if e.UNLocationCode != port || e.EventTypeCode != code {
			continue
		}
		if r := rank[e.EventClassifierCode]; r >= bestRank && r > 0 {
			best, bestRank = Moment{At: e.EventDateTime, Kind: kinds[e.EventClassifierCode], Source: e.EventID}, r
		}
	}
	return best
}

// position follows the latest actual event: a departure puts the shipment
// at sea toward the next port (estimated); anything else, at its port.
func position(events []intents.TnTEventBody, route []string, pol string) Position {
	var last *intents.TnTEventBody
	for i := range events {
		e := &events[i]
		if e.EventClassifierCode == intents.ClassifierActual && (last == nil || e.EventDateTime >= last.EventDateTime) {
			last = e
		}
	}
	if last == nil {
		p := point(pol)
		return Position{At: &p}
	}
	if last.EventTypeCode == "DEPA" {
		if i := slices.Index(route, last.UNLocationCode); i >= 0 && i+1 < len(route) {
			return Position{Between: [2]string{route[i], route[i+1]}, Estimated: true, Since: last.EventID}
		}
	}
	p := point(last.UNLocationCode)
	return Position{At: &p, Since: last.EventID}
}
