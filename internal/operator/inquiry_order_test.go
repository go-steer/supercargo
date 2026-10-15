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

package operator

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
)

// d2dLegs is a door-to-door order's plan: a truck, a ship, a truck.
var d2dLegs = []intents.PlannedLeg{
	{Mode: intents.ModeRoad, From: "NLTLB", To: "NLRTM", Carrier: "Polder Haulage"},
	{Mode: intents.ModeOcean, From: "NLRTM", To: "BRSSZ", Carrier: "Ocean Blue", Vessel: "Ocean Blue Aurora 42W"},
	{Mode: intents.ModeRoad, From: "BRSSZ", To: "BRSAO", Carrier: "Serra Transportes"},
}

func d2dEvent(id, code, class, loc string, src ...intents.Provenance) store.Record {
	raw, _ := json.Marshal(intents.TnTEventBody{EventID: id, EventType: intents.EventTypeTransport, EventTypeCode: code, EventClassifierCode: class,
		EventDateTime: "2026-10-15T11:00:00Z", UNLocationCode: loc, Description: "the carrier's own words", DerivedFrom: src})
	return store.Record{ID: id, Intent: intents.TnTEvent, Body: raw}
}

var (
	edi    = intents.Provenance{Source: "EDIFACT IFTSTA from polder-haulage.example", Reference: "PH4471", Signed: false}
	signed = intents.Provenance{ID: "EV-000003", Signed: true}
)

// withLegs sets each view's leg, as the inquiry service does from the relay.
func withLegs(views []recordView, legs ...int) []recordView {
	for i := range views {
		views[i].Leg = legs[i]
	}
	return views
}

// TestViewRecordsSource: the model learns whether a re-issued record was
// signed, as the code decides it, and none of derived_from's text.
func TestViewRecordsSource(t *testing.T) {
	alert, _ := json.Marshal(intents.TnTAlertBody{AlertID: "MF-AL-1", Kind: intents.AlertDelay, Severity: intents.SeverityHigh, Summary: "Later.",
		PreviousETA: "2026-11-04T02:00:00Z", NewETA: "2026-11-07T02:00:00Z", DerivedFrom: []intents.Provenance{{ID: "AL-000006", Signed: true}}})
	views := viewRecords([]store.Record{
		d2dEvent("MF-EV-1", "DEPA", intents.ClassifierActual, "NLTLB", edi),
		d2dEvent("MF-EV-2", "DEPA", intents.ClassifierActual, "NLRTM", signed),
		d2dEvent("MF-EV-3", "DEPA", intents.ClassifierActual, "NLRTM", signed, edi), // any unsigned source makes it unsigned
		d2dEvent("EV-4", "DEPA", intents.ClassifierActual, "NLRTM"),                 // the operator's own
		{ID: "MF-AL-1", Intent: intents.TnTAlert, Body: alert},
	})
	want := []string{SourceUnsigned, SourceSigned, SourceUnsigned, "", SourceSigned}
	for i, v := range views {
		if v.Source != want[i] {
			t.Errorf("record %d: source %q, want %q", i, v.Source, want[i])
		}
	}
	// Nothing of derived_from reaches the prompt: not its source, not its reference.
	b, _ := json.Marshal(views)
	for _, s := range []string{"polder-haulage", "PH4471", "EV-000003", "AL-000006", "derived_from"} {
		if strings.Contains(string(b), s) {
			t.Errorf("the model would see %q: %s", s, b)
		}
	}
	if views[3].Event.Description != "the carrier's own words" {
		t.Error("the operator's own event lost its description")
	}
}

// TestViewBookingOrder: an order's booking view has its status, plan,
// cargo and legs, without the legs' vessels (a leg carrier's text).
func TestViewBookingOrder(t *testing.T) {
	b := store.Booking{Ref: "MF-0007", Order: &store.OrderBooking{
		Request: intents.ForwardingRequestBody{PlaceOfReceipt: intents.Place{UNLocationCode: "NLTLB"}, PlaceOfDelivery: intents.Place{UNLocationCode: "BRSAO"},
			Cargo: intents.Cargo{Description: "Footwear"}},
		Confirmation: intents.ForwardingConfirmationBody{Status: intents.StatusConfirmed, PlannedDelivery: "2026-11-04T02:00:00Z",
			Legs: append([]intents.PlannedLeg{{Mode: intents.ModeRoad, From: "NLTLB", To: "NLRTM", PlannedDeparture: "2026-10-15T11:00:00Z"}}, d2dLegs[1:]...)}}}
	v := viewBooking(b)
	if v.Status != intents.StatusConfirmed || v.PlannedArrival != "2026-11-04T02:00:00Z" || v.PlannedDeparture != "2026-10-15T11:00:00Z" ||
		v.Cargo != "Footwear" || len(v.Legs) != 3 || v.Origin.UNLocode != "NLTLB" || v.Destination.UNLocode != "BRSAO" {
		t.Errorf("view %+v", v)
	}
	if v.Legs[1].Vessel != "" || b.Order.Confirmation.Legs[1].Vessel == "" {
		t.Errorf("the vessel: in the view %q; in the order %q", v.Legs[1].Vessel, b.Order.Confirmation.Legs[1].Vessel)
	}
	if v := viewBooking(store.Booking{Ref: "OB-1"}); v.Legs != nil {
		t.Errorf("an ocean booking has legs: %+v", v)
	}
}

// TestOrderAnswer: the template's answer for an order names the latest
// event's leg (the relay's), marks it unsigned if it is, gives the
// delivery as the latest alert moved it, and counts the unsigned events
// by leg.
func TestOrderAnswer(t *testing.T) {
	b := bookingView{BookingRef: "MF-0007", Destination: intents.Location{UNLocode: "BRSAO", Name: "São Paulo"}, PlannedArrival: "2026-11-04T02:00:00Z", Legs: d2dLegs}
	// No events yet: the journey and the planned delivery.
	if got, cited := templateAnswer(b, nil); !strings.Contains(got, "NLTLB → NLRTM by road, NLRTM → BRSSZ by sea") || !strings.Contains(got, "2026-11-04T02:00:00Z") || len(cited) != 0 {
		t.Errorf("no events: %q %v", got, cited)
	}
	recs := withLegs(viewRecords([]store.Record{
		d2dEvent("MF-EV-1", "DEPA", intents.ClassifierActual, "NLTLB", edi),
		d2dEvent("MF-EV-2", "GTIN", intents.ClassifierActual, "NLRTM", edi),
	}), 1, 1)
	got, cited := templateAnswer(b, recs)
	for _, want := range []string{"(leg 1, by road with Polder Haulage)", "MF-EV-2, unsigned", "2 of the 2 events are EDI statuses (leg 1)", "0 are re-issued"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if len(cited) != 1 || cited[0] != "MF-EV-2" {
		t.Errorf("cited %v", cited)
	}
	// The ship's signed events, a delay.
	alert, _ := json.Marshal(intents.TnTAlertBody{AlertID: "MF-AL-1", Kind: intents.AlertDelay, Severity: intents.SeverityHigh, Summary: "The ship is late.",
		PreviousETA: "2026-11-04T02:00:00Z", NewETA: "2026-11-07T02:00:00Z"})
	more := withLegs(viewRecords([]store.Record{
		d2dEvent("MF-EV-3", "LOAD", intents.ClassifierActual, "NLRTM", signed),
		{ID: "MF-AL-1", Intent: intents.TnTAlert, Body: alert},
	}), 2, 2)
	got, cited = templateAnswer(b, append(recs, more...))
	if !strings.Contains(got, "(leg 2, by sea with Ocean Blue)") || strings.Contains(got, "MF-EV-3, unsigned") ||
		!strings.Contains(got, "now expected 2026-11-07T02:00:00Z") || !strings.Contains(got, "2 of the 3 events") || !strings.Contains(got, "1 are re-issued") || len(cited) != 2 {
		t.Errorf("delayed: %q %v", got, cited)
	}
	// Delivered, reported only over EDI (a drop at the door): said, and marked.
	drop := withLegs(viewRecords([]store.Record{d2dEvent("MF-EV-9", "DROP", intents.ClassifierActual, "BRSAO", edi)}), 3)
	last := withLegs(viewRecords([]store.Record{d2dEvent("MF-EV-10", "GTOT", intents.ClassifierActual, "BRSAO", signed)}), 3)
	got, cited = templateAnswer(b, append(append(append(recs, more...), drop...), last...))
	if !strings.Contains(got, "It was delivered at BRSAO on 2026-10-15T11:00:00Z (MF-EV-9, unsigned") || strings.Contains(got, "now expected") ||
		!strings.Contains(got, "(legs 1, 3)") || len(cited) != 3 {
		t.Errorf("delivered: %q %v", got, cited)
	}
	// A record whose leg the relay doesn't know: "the order", and "others".
	unknown := viewRecords([]store.Record{d2dEvent("MF-EV-11", "ARRI", intents.ClassifierActual, "NLRTM", edi)})
	got, _ = templateAnswer(b, append(recs, unknown...))
	if !strings.Contains(got, "Latest event (the order)") || !strings.Contains(got, "(legs 1, others)") {
		t.Errorf("unknown leg: %q", got)
	}
	if got, _ := templateAnswer(b, unknown); !strings.Contains(got, "1 of the 1 events are EDI statuses that") {
		t.Errorf("only unknown legs: %q", got)
	}
	// All signed: no count.
	if got, _ := templateAnswer(b, more); strings.Contains(got, "EDI") {
		t.Errorf("all signed: %q", got)
	}
}

// TestOrderAnswerLength: with every text at its longest and eight legs,
// the template's answer still fits the response's limit, so the asker's
// node takes it.
func TestOrderAnswerLength(t *testing.T) {
	long := strings.Repeat("Ó", 128)
	var legs []intents.PlannedLeg
	for range intents.MaxLegs {
		legs = append(legs, intents.PlannedLeg{Mode: intents.ModeRoad, From: "NLTLB", To: "BRSAO", Carrier: long})
	}
	b := bookingView{BookingRef: "MF-0007", Destination: intents.Location{UNLocode: "BRSAO", Name: long}, PlannedArrival: "2026-11-04T02:00:00Z", Legs: legs}
	var recs []store.Record
	for i := range intents.MaxLegs {
		raw, _ := json.Marshal(intents.TnTEventBody{EventID: "MF-EV-" + strings.Repeat("9", 50) + string(rune('A'+i)), EventType: intents.EventTypeTransport,
			EventTypeCode: "DEPA", EventClassifierCode: intents.ClassifierActual, EventDateTime: "2026-10-15T11:00:00Z", UNLocationCode: "NLTLB",
			FacilityName: long, DerivedFrom: []intents.Provenance{edi}})
		recs = append(recs, store.Record{Intent: intents.TnTEvent, Body: raw})
	}
	alert, _ := json.Marshal(intents.TnTAlertBody{AlertID: "MF-AL-" + strings.Repeat("9", 50), Kind: intents.AlertDelay, Severity: intents.SeverityHigh,
		Summary: strings.Repeat("Ü", 280), PreviousETA: "2026-11-04T02:00:00Z", NewETA: "2026-11-07T02:00:00Z"})
	recs = append(recs, store.Record{Intent: intents.TnTAlert, Body: alert})
	views := viewRecords(recs)
	for i := range views[:intents.MaxLegs] {
		views[i].Leg = i + 1
	}
	got, cited := templateAnswer(b, views)
	resp := intents.TnTInquiryResponseBody{Answer: got, CitedEventIDs: cited, GeneratedBy: intents.GeneratedByTemplate,
		DataScope: intents.DataScope{BookingRef: "MF-0007", EventsConsidered: len(views)}}
	if err := resp.Validate(); err != nil {
		t.Errorf("the template's answer doesn't validate: %v (%d characters)", err, len([]rune(got)))
	}
	// A single sentence over the limit is cut short too.
	if got := capAnswer(strings.Repeat("x", intents.MaxAnswerLen+10)); len([]rune(got)) != intents.MaxAnswerLen || !strings.HasSuffix(got, "…") {
		t.Errorf("one long sentence: %d characters", len([]rune(got)))
	}
}

// TestInquiryLegOf: the service takes each re-issued record's leg from
// the relay, only within the order's legs, and an order's inquiry is
// still refused to anyone but its shipper.
func TestInquiryLegOf(t *testing.T) {
	bookings, log := store.NewBookings(), store.NewEventLog(0)
	b := store.Booking{Ref: "MF-0007", Operator: "meridian-forwarding.example", Shipper: "cymbal-shoes.example", Order: &store.OrderBooking{
		Request:      intents.ForwardingRequestBody{PlaceOfReceipt: intents.Place{UNLocationCode: "NLTLB"}, PlaceOfDelivery: intents.Place{UNLocationCode: "BRSAO"}},
		Confirmation: intents.ForwardingConfirmationBody{Status: intents.StatusConfirmed, PlannedDelivery: "2026-11-04T02:00:00Z", Legs: d2dLegs}}}
	_ = bookings.Add(b)
	for _, r := range []store.Record{
		d2dEvent("MF-EV-1", "DEPA", intents.ClassifierActual, "NLTLB", edi),
		d2dEvent("MF-EV-2", "DEPA", intents.ClassifierActual, "NLRTM", edi), // the relay says leg 9: out of range, ignored
	} {
		_, _ = log.Add(b.Operator, b.Ref, r)
	}
	s := &InquiryService{Bookings: bookings, Log: log, Name: "Meridian",
		LegOf: func(id string) (int, bool) { return map[string]int{"MF-EV-1": 1, "MF-EV-2": 9}[id], true }}
	ask := func(sender string) (*node.Reply, error) {
		h := envelope.NewHeader(intents.TnTInquiry, sender, b.Operator, time.Now())
		ref := b.Ref
		h.BookingRef = &ref
		req := &node.Request{Envelope: &envelope.Envelope{Header: h}, Body: &intents.TnTInquiryBody{Question: "Where is it?"}}
		if err := s.Authorize(context.Background(), req); err != nil {
			return nil, err
		}
		return s.Handle(context.Background(), req)
	}
	if _, err := ask("ocean-blue.example"); err == nil {
		t.Error("another company asked about the order")
	} else if rej := (*node.Rejection)(nil); !errors.As(err, &rej) {
		t.Errorf("refused as %v", err)
	}
	reply, err := ask("cymbal-shoes.example")
	if err != nil {
		t.Fatal(err)
	}
	got := reply.Body.(*intents.TnTInquiryResponseBody).Answer
	if !strings.Contains(got, "Latest event (the order)") || !strings.Contains(got, "(legs 1, others)") {
		t.Errorf("answer %q", got)
	}
}
