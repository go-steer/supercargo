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
	// Truncated says some ports off the plan were left out (MaxExtraPorts).
	Truncated bool `json:"truncated,omitempty"`
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
	FromPort Point  `json:"from"`
	ToPort   Point  `json:"to"`
	Mode     string `json:"mode"` // "ocean" or "road"
	// Carrier is who carries the leg, as an order's plan names it.
	Carrier   string `json:"carrier,omitempty"`
	Vessel    string `json:"vessel,omitempty"`
	Voyage    string `json:"voyage,omitempty"`
	Departure Moment `json:"departure,omitzero"` // none: no event, and no plan for this leg
	Arrival   Moment `json:"arrival,omitzero"`
	// PlannedArrival is the plan's arrival, when Arrival has moved off it.
	PlannedArrival string `json:"plannedArrival,omitempty"`
	Status         string `json:"status"`
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

// MaxExtraPorts bounds the ports off the plan a journey draws: the events
// are an operator's, and a hostile one could report thousands of places.
const MaxExtraPorts = 16

var (
	classifierRank = map[string]int{intents.ClassifierPlanned: 1, intents.ClassifierEstimated: 2, intents.ClassifierActual: 3}
	momentKind     = map[string]string{intents.ClassifierPlanned: MomentPlanned, intents.ClassifierEstimated: MomentEstimated, intents.ClassifierActual: MomentActual}
)

// BuildJourney derives b's journey from its records (as Summarize does
// its summary): the plan comes from the booking, everything else from the
// events and alerts held.
func BuildJourney(b Booking, recs []Record) Journey {
	c := b.Confirmation
	if r := b.Road; r != nil {
		// A truck leg's plan: from pickup to delivery.
		c.PlannedDeparture, c.PlannedArrival = r.Confirmation.PlannedPickup, r.Confirmation.PlannedDelivery
	}
	if o := b.Order; o != nil {
		// An order's plan, door to door.
		c.PlannedArrival = o.Confirmation.PlannedDelivery
		if legs := o.Confirmation.Legs; len(legs) > 0 {
			c.PlannedDeparture = legs[0].PlannedDeparture
		}
	}
	pol, pod := b.Places()
	j := Journey{Booking: b.Ref, Operator: b.Operator, Shipper: b.Shipper, Legs: []Leg{}, Calls: []Call{}, Alerts: []AlertRef{}}
	events, alerts, held := journeyRecords(recs)
	if b.Order != nil && len(b.Order.Confirmation.Legs) > 0 {
		return orderJourney(j, b.Order.Confirmation, events, alerts, held)
	}

	// The ports, in order: loading, the ports off the plan where something
	// actually happened (by when it first did; at most MaxExtraPorts),
	// discharge. A plan or an estimate elsewhere doesn't redraw the route.
	extra := map[string]string{} // port → its earliest actual event
	for _, e := range events {
		if p := e.UNLocationCode; p != pol && p != pod && e.EventClassifierCode == intents.ClassifierActual {
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
	if len(mid) > MaxExtraPorts {
		mid, j.Truncated = mid[:MaxExtraPorts], true
	}
	route := append(append([]string{pol}, mid...), pod)
	onRoute := map[string]bool{}
	for _, p := range route {
		onRoute[p] = true
	}

	// One pass over the events: each port's, and the firmest, latest
	// moment of each code there.
	atPort := map[string][]intents.TnTEventBody{}
	moments := map[[2]string]Moment{}
	ranks := map[[2]string]int{}
	for _, e := range events {
		if !onRoute[e.UNLocationCode] {
			continue
		}
		atPort[e.UNLocationCode] = append(atPort[e.UNLocationCode], e)
		key := [2]string{e.UNLocationCode, e.EventTypeCode}
		if r := classifierRank[e.EventClassifierCode]; r > 0 && r >= ranks[key] {
			moments[key], ranks[key] = Moment{At: e.EventDateTime, Kind: momentKind[e.EventClassifierCode], Source: e.EventID}, r
		}
	}

	for _, p := range route {
		call := Call{Port: point(p), Kinds: []string{}, Events: []string{}}
		if _, off := extra[p]; off {
			call.Kinds = append(call.Kinds, "unscheduled")
		}
		for _, e := range atPort[p] {
			if k, ok := callKinds[e.EventTypeCode]; ok && e.EventClassifierCode == intents.ClassifierActual && !slices.Contains(call.Kinds, k) {
				call.Kinds = append(call.Kinds, k)
			}
			call.Events = append(call.Events, e.EventID)
		}
		if len(call.Events) > 0 {
			j.Calls = append(j.Calls, call)
		}
	}

	vessel, voyage := c.Vessel()
	for i := 0; i+1 < len(route); i++ {
		from, to := route[i], route[i+1]
		leg := Leg{FromPort: point(from), ToPort: point(to), Mode: "ocean", Vessel: vessel, Voyage: voyage, Sources: []string{}}
		leg.Departure = moments[[2]string{from, "DEPA"}]
		if leg.Departure.At == "" && i == 0 && c.PlannedDeparture != "" {
			leg.Departure = Moment{At: c.PlannedDeparture, Kind: MomentPlanned}
		}
		leg.Arrival = moments[[2]string{to, "ARRI"}]
		last := i == len(route)-2
		if leg.Arrival.At == "" && last && c.PlannedArrival != "" {
			leg.Arrival = Moment{At: c.PlannedArrival, Kind: MomentPlanned}
		}
		for _, m := range []Moment{leg.Departure, leg.Arrival} {
			if m.Source != "" {
				leg.Sources = append(leg.Sources, m.Source)
				leg.Standard = intents.DialectDCSATnT.Standard
			}
		}
		// Done once anything actually happened at its far end (an arrival,
		// or a load or discharge there).
		switch {
		case leg.Arrival.Kind == MomentActual || lastActual(atPort[to]) != "":
			leg.Status = LegDone
		case last && c.PlannedArrival != "" && leg.Arrival.Kind == MomentEstimated && leg.Arrival.At > c.PlannedArrival:
			leg.Status = LegDelayed
		case leg.Departure.Kind == MomentActual:
			leg.Status = LegInProgress
		default:
			leg.Status = LegPlanned
		}
		j.Legs = append(j.Legs, leg)
	}

	j.Position = position(j.Legs, atPort)
	j.Alerts = alertRefs(alerts, held, len(j.Legs)-1)
	return j
}

// journeyRecords parses a booking's events and alerts, and which event
// IDs it holds.
func journeyRecords(recs []Record) ([]intents.TnTEventBody, []intents.TnTAlertBody, map[string]bool) {
	var events []intents.TnTEventBody
	var alerts []intents.TnTAlertBody
	held := map[string]bool{}
	for _, r := range recs {
		switch r.Intent {
		case intents.TnTEvent:
			var e intents.TnTEventBody
			if json.Unmarshal(r.Body, &e) == nil { // the node's own, validated when stored
				events = append(events, e)
				held[e.EventID] = true
			}
		case intents.TnTAlert:
			var a intents.TnTAlertBody
			if json.Unmarshal(r.Body, &a) == nil {
				alerts = append(alerts, a)
			}
		}
	}
	return events, alerts, held
}

// alertRefs are the alerts on a journey, each on leg.
func alertRefs(alerts []intents.TnTAlertBody, held map[string]bool, leg int) []AlertRef {
	out := []AlertRef{}
	for _, a := range alerts {
		ref := AlertRef{ID: a.AlertID, Kind: a.Kind, Severity: a.Severity, PreviousETA: a.PreviousETA, NewETA: a.NewETA, Leg: leg}
		// Only causes this node holds: a view links each one to a message.
		for _, id := range a.CauseEventIDs {
			if held[id] {
				ref.Causes = append(ref.Causes, id)
			}
		}
		out = append(out, ref)
	}
	return out
}

// orderJourney is a door-to-door order's journey: the legs its forwarder
// planned, each with its mode and carrier; the plan's times until events
// replace them; and the delivery as the order's latest delay or early
// alert moved it. The route is the plan's: an event elsewhere (a ship's
// unscheduled call) stays in the events, off the strip.
func orderJourney(j Journey, c intents.ForwardingConfirmationBody, events []intents.TnTEventBody, alerts []intents.TnTAlertBody, held map[string]bool) Journey {
	route := []string{c.Legs[0].From}
	for _, l := range c.Legs {
		route = append(route, l.To)
	}
	atPort := map[string][]intents.TnTEventBody{}
	moments := map[[2]string]Moment{}
	ranks := map[[2]string]int{}
	for _, e := range events {
		if !slices.Contains(route, e.UNLocationCode) {
			continue
		}
		atPort[e.UNLocationCode] = append(atPort[e.UNLocationCode], e)
		key := [2]string{e.UNLocationCode, e.EventTypeCode}
		if r := classifierRank[e.EventClassifierCode]; r > 0 && r >= ranks[key] {
			moments[key], ranks[key] = Moment{At: e.EventDateTime, Kind: momentKind[e.EventClassifierCode], Source: e.EventID}, r
		}
	}
	seen := map[string]bool{}
	for _, p := range route {
		if seen[p] {
			continue
		}
		seen[p] = true
		call := Call{Port: point(p), Kinds: []string{}, Events: []string{}}
		for _, e := range atPort[p] {
			if k, ok := callKinds[e.EventTypeCode]; ok && e.EventClassifierCode == intents.ClassifierActual && !slices.Contains(call.Kinds, k) {
				call.Kinds = append(call.Kinds, k)
			}
			call.Events = append(call.Events, e.EventID)
		}
		if len(call.Events) > 0 {
			j.Calls = append(j.Calls, call)
		}
	}
	var moved Moment
	for _, a := range alerts {
		if (a.Kind == intents.AlertDelay || a.Kind == intents.AlertEarly) && a.NewETA != "" {
			moved = Moment{At: a.NewETA, Kind: MomentEstimated, Source: a.AlertID}
		}
	}
	for i, pl := range c.Legs {
		if i == len(c.Legs)-1 && pl.PlannedArrival == "" {
			pl.PlannedArrival = c.PlannedDelivery // the last leg ends with the delivery
		}
		leg := Leg{FromPort: point(pl.From), ToPort: point(pl.To), Mode: pl.Mode, Carrier: pl.Carrier, Vessel: pl.Vessel, Sources: []string{}}
		leg.Departure = moments[[2]string{pl.From, "DEPA"}]
		if leg.Departure.At == "" && pl.PlannedDeparture != "" {
			leg.Departure = Moment{At: pl.PlannedDeparture, Kind: MomentPlanned}
		}
		leg.Arrival = moments[[2]string{pl.To, "ARRI"}]
		if i == len(c.Legs)-1 && moved.At != "" && leg.Arrival.Kind != MomentActual {
			leg.Arrival = moved
		}
		if leg.Arrival.At == "" && pl.PlannedArrival != "" {
			leg.Arrival = Moment{At: pl.PlannedArrival, Kind: MomentPlanned}
		}
		if pl.PlannedArrival != "" && leg.Arrival.At != pl.PlannedArrival {
			leg.PlannedArrival = pl.PlannedArrival
		}
		for _, m := range []Moment{leg.Departure, leg.Arrival} {
			if held[m.Source] {
				leg.Sources = append(leg.Sources, m.Source)
			}
		}
		// Done once it actually arrived, or anything actually happened at
		// its far end after it actually left (an event there before then
		// is another leg's, as on a round trip); delayed once its arrival
		// is estimated past the plan's.
		switch {
		case leg.Arrival.Kind == MomentActual || leg.Departure.Kind == MomentActual && actualSince(atPort[pl.To], leg.Departure.At):
			leg.Status = LegDone
		case pl.PlannedArrival != "" && leg.Arrival.Kind == MomentEstimated && leg.Arrival.At > pl.PlannedArrival:
			leg.Status = LegDelayed
		case leg.Departure.Kind == MomentActual:
			leg.Status = LegInProgress
		default:
			leg.Status = LegPlanned
		}
		j.Legs = append(j.Legs, leg)
	}
	j.Position = position(j.Legs, atPort)
	j.Alerts = alertRefs(alerts, held, len(j.Legs)-1)
	return j
}

// position follows the legs, so the two never disagree: at sea on the
// first leg not done if it actually departed (an estimate), else at its
// port of departure; at the end if every leg is done.
func position(legs []Leg, atPort map[string][]intents.TnTEventBody) Position {
	for _, l := range legs {
		if l.Status == LegDone {
			continue
		}
		if l.Departure.Kind == MomentActual {
			return Position{Between: [2]string{l.FromPort.UNLocode, l.ToPort.UNLocode}, Estimated: true, Since: l.Departure.Source}
		}
		at := l.FromPort
		return Position{At: &at, Since: lastActual(atPort[at.UNLocode])}
	}
	end := legs[len(legs)-1].ToPort
	return Position{At: &end, Since: lastActual(atPort[end.UNLocode])}
}

// actualSince reports whether an actual event among events happened at or
// after at.
func actualSince(events []intents.TnTEventBody, at string) bool {
	return slices.ContainsFunc(events, func(e intents.TnTEventBody) bool {
		return e.EventClassifierCode == intents.ClassifierActual && e.EventDateTime >= at
	})
}

// lastActual is the latest actual event among events, if any.
func lastActual(events []intents.TnTEventBody) string {
	id, at := "", ""
	for _, e := range events {
		if e.EventClassifierCode == intents.ClassifierActual && e.EventDateTime >= at {
			id, at = e.EventID, e.EventDateTime
		}
	}
	return id
}
