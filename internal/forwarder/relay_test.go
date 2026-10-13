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

package forwarder

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/intents"
)

// published records what the relay publishes on orders.
type published struct {
	bodies []intents.Body
	fail   error
}

func (p *published) Publish(_ store.Booking, body intents.Body) error {
	if p.fail != nil {
		return p.fail
	}
	if err := body.Validate(); err != nil {
		return err
	}
	p.bodies = append(p.bodies, body)
	return nil
}

// amender is a road carrier answering amendments.
type amender struct {
	answer *intents.RoadBookingConfirmationBody
	err    error
	got    *intents.RoadBookingAmendmentBody
}

func (a *amender) AmendRoad(_ context.Context, _, _ string, am *intents.RoadBookingAmendmentBody) (*intents.RoadBookingConfirmationBody, error) {
	a.got = am
	return a.answer, a.err
}

// relayed is an order booked on Ocean Blue (OB-BKG-1042, arriving the
// 23rd at 14:00, ready for the truck the 24th at 20:00) and Serra
// (SR-BKG-0913, collecting a day later, the 25th at 20:00, delivering the
// 27th at 02:00), and a relay for it.
func relayed(t *testing.T) (*Relay, *published, *amender) {
	t.Helper()
	o, _ := desk(t)
	if _, err := o.Handle(context.Background(), orderFrom(cymbal, doorOrder())); err != nil {
		t.Fatal(err)
	}
	_, _ = o.Bookings.Update(meridian, "MF-0007", func(b *store.Booking) error {
		b.Order.Confirmation = intents.ForwardingConfirmationBody{OrderRef: "MF-0007", Status: intents.StatusConfirmed, PlannedDelivery: "2026-10-27T02:00:00Z"}
		b.Order.Legs = []store.LegRef{
			{Mode: intents.ModeOcean, Carrier: "ocean-blue.example", Name: "Ocean Blue", Ref: "OB-BKG-1042", From: "NLRTM", To: "BRSSZ",
				Departure: "2026-10-07T18:00:00Z", Arrival: "2026-10-23T14:00:00Z", Containers: []string{"OCBU0000004"}},
			{Mode: intents.ModeRoad, Carrier: "serra-transportes.example", Name: "Serra Transportes", Ref: "SR-BKG-0913", From: "BRSSZ", To: "BRSAO",
				Departure: "2026-10-25T20:00:00Z", Arrival: "2026-10-27T02:00:00Z"},
		}
		return nil
	})
	pub, am := &published{}, &amender{}
	return &Relay{Orders: o, Publish: pub, Amend: am, Now: time.Now}, pub, am
}

var (
	shipLeg  = store.Booking{Ref: "OB-BKG-1042", Operator: "ocean-blue.example"}
	truckLeg = store.Booking{Ref: "SR-BKG-0913", Operator: "serra-transportes.example"}
)

func event(id, desc string) *intents.TnTEventBody {
	return &intents.TnTEventBody{EventID: id, EventType: intents.EventTypeTransport, EventClassifierCode: intents.ClassifierEstimated,
		EventTypeCode: "ARRI", EventDateTime: "2026-10-26T14:00:00Z", Description: desc, UNLocationCode: "BRSSZ"}
}

func delay(id, cause, prev, next string) *intents.TnTAlertBody {
	return &intents.TnTAlertBody{AlertID: id, Kind: intents.AlertDelay, Severity: intents.SeverityHigh, Summary: "Engine repair.",
		PreviousETA: prev, NewETA: next, CauseEventIDs: []string{cause}}
}

// TestRelayEvent: a leg's event is re-issued as its order's, derived from
// it, saying which leg; an event of a booking that's no order's leg isn't.
func TestRelayEvent(t *testing.T) {
	r, pub, _ := relayed(t)
	if err := r.Event(store.Booking{Ref: "X-1", Operator: "ocean-blue.example"}, event("EV-1", "Elsewhere")); err != nil || len(pub.bodies) != 0 {
		t.Fatalf("not a leg: %v, %d published", err, len(pub.bodies))
	}
	if err := r.Event(shipLeg, event("EV-000007", "Ocean Blue Aurora 42W expected at Port of Santos")); err != nil {
		t.Fatal(err)
	}
	e := pub.bodies[0].(*intents.TnTEventBody)
	if e.EventID != "MF-EV-000001" || e.Description != "Ocean Blue Aurora 42W expected at Port of Santos (leg 1, Ocean Blue)" ||
		len(e.DerivedFrom) != 1 || e.DerivedFrom[0] != (intents.Provenance{ID: "EV-000007", Signed: true}) || e.UNLocationCode != "BRSSZ" {
		t.Errorf("relayed %+v", e)
	}
	pub.fail = errors.New("closed")
	if err := r.Event(shipLeg, event("EV-000008", "x")); err == nil {
		t.Error("a publish failure")
	}
}

// TestRelayDelay: a ship's delay moves the truck's collection, so the
// order's delivery moves by as much: the order's alert says so, citing
// the relayed estimate, derived from the ship's alert, and a proposal to
// re-time the truck waits; nothing is sent to the truck's carrier.
func TestRelayDelay(t *testing.T) {
	r, pub, am := relayed(t)
	var told []Proposal
	r.OnProposal = func(p Proposal) { told = append(told, p) }
	_ = r.Event(shipLeg, event("EV-000007", "Ocean Blue Aurora 42W expected at Port of Santos"))
	if err := r.Alert(shipLeg, delay("AL-000008", "EV-000007", "2026-10-23T14:00:00Z", "2026-10-26T14:00:00Z")); err != nil {
		t.Fatal(err)
	}
	a := pub.bodies[1].(*intents.TnTAlertBody)
	if a.Kind != intents.AlertDelay || a.PreviousETA != "2026-10-27T02:00:00Z" || a.NewETA != "2026-10-29T02:00:00Z" ||
		!strings.Contains(a.Summary, "by 48 h") ||
		a.CauseEventIDs[0] != "MF-EV-000001" || a.DerivedFrom[0].ID != "AL-000008" || strings.Contains(a.Summary, "Engine") {
		t.Errorf("the order's alert %+v", a)
	}
	ps := r.Proposals("MF-0007")
	if len(ps) != 1 || len(told) != 1 || am.got != nil {
		t.Fatalf("proposals %+v; sent %+v", ps, am.got)
	}
	p := ps[0]
	if p.Leg != 2 || p.LegRef != "SR-BKG-0913" || p.Amendment.Pickup.Earliest != "2026-10-27T20:00:00Z" || p.Amendment.Delivery.Latest != "2026-10-29T02:00:00Z" {
		t.Errorf("proposal %+v", p)
	}
	if len(r.Proposals("MF-9999")) != 0 {
		t.Error("another order's proposals")
	}
}

// TestRelayAlertKinds: a delay the plan absorbs, an early arrival, the
// last leg's delay (moving the delivery, or not), and an alert citing
// nothing relayed.
func TestRelayAlertKinds(t *testing.T) {
	r, pub, _ := relayed(t)
	_ = r.Event(shipLeg, event("EV-1", "est"))
	_ = r.Event(truckLeg, event("EV-2", "est"))
	for name, tc := range map[string]struct {
		leg   store.Booking
		alert *intents.TnTAlertBody
		kind  string
		eta   string
	}{
		"absorbed by the slack": {shipLeg, delay("AL-1", "EV-1", "2026-10-23T14:00:00Z", "2026-10-23T16:00:00Z"), intents.AlertException, ""},
		"an early ship": {shipLeg, &intents.TnTAlertBody{AlertID: "AL-2", Kind: intents.AlertEarly, Severity: intents.SeverityLow, Summary: "Fast.",
			PreviousETA: "2026-10-23T14:00:00Z", NewETA: "2026-10-22T14:00:00Z", CauseEventIDs: []string{"EV-1"}}, intents.AlertEarly, "2026-10-22T14:00:00Z"},
		"the truck late":       {truckLeg, delay("AL-3", "EV-2", "2026-10-27T02:00:00Z", "2026-10-27T08:00:00Z"), intents.AlertDelay, "2026-10-27T08:00:00Z"},
		"the truck still fine": {truckLeg, delay("AL-4", "EV-2", "2026-10-25T02:00:00Z", "2026-10-25T08:00:00Z"), intents.AlertException, ""},
	} {
		n := len(pub.bodies)
		if err := r.Alert(tc.leg, tc.alert); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		a := pub.bodies[n].(*intents.TnTAlertBody)
		if a.Kind != tc.kind || a.NewETA != tc.eta {
			t.Errorf("%s: %+v", name, a)
		}
	}
	if len(r.Proposals("MF-0007")) != 0 {
		t.Error("a proposal without a re-timing")
	}
	if err := r.Alert(shipLeg, delay("AL-9", "EV-404", "2026-10-23T14:00:00Z", "2026-10-26T14:00:00Z")); err == nil {
		t.Error("an alert citing nothing relayed")
	}
	if err := r.Alert(store.Booking{Ref: "X", Operator: "y.example"}, delay("AL-9", "EV-1", "2026-10-23T14:00:00Z", "2026-10-26T14:00:00Z")); err != nil {
		t.Errorf("not a leg: %v", err)
	}
	pub.fail = errors.New("closed")
	if err := r.Alert(shipLeg, delay("AL-10", "EV-1", "2026-10-23T14:00:00Z", "2026-10-26T14:00:00Z")); err == nil {
		t.Error("a publish failure")
	}
}

// TestConfirmProposal: a confirmed proposal's amendment goes to the
// truck's carrier; its new plan replaces the leg's in the order; the
// proposal closes, and can't be confirmed twice.
func TestConfirmProposal(t *testing.T) {
	r, _, am := relayed(t)
	_ = r.Event(shipLeg, event("EV-1", "est"))
	_ = r.Alert(shipLeg, delay("AL-1", "EV-1", "2026-10-23T14:00:00Z", "2026-10-26T14:00:00Z"))
	id := r.Proposals("MF-0007")[0].ID
	am.answer = &intents.RoadBookingConfirmationBody{BookingRef: "SR-BKG-0913", Status: intents.StatusConfirmed,
		PlannedPickup: "2026-10-27T20:00:00Z", PlannedDelivery: "2026-10-29T02:00:00Z"}
	p, err := r.Confirm(context.Background(), id)
	if err != nil || p.Status != ProposalConfirmed || am.got.Pickup.Earliest != "2026-10-27T20:00:00Z" {
		t.Fatalf("%+v, %v", p, err)
	}
	b, _ := r.Orders.Bookings.Get(meridian, "MF-0007")
	if b.Order.Legs[1].Departure != "2026-10-27T20:00:00Z" || b.Order.Legs[1].Arrival != "2026-10-29T02:00:00Z" {
		t.Errorf("leg 2 %+v", b.Order.Legs[1])
	}
	if _, err := r.Confirm(context.Background(), id); !errors.Is(err, ErrProposalClosed) {
		t.Errorf("twice: %v", err)
	}
	if _, err := r.Confirm(context.Background(), "MF-PR-999999"); !errors.Is(err, ErrUnknownProposal) {
		t.Errorf("unknown: %v", err)
	}
}

// TestConfirmProposalOutcomes: a carrier refusing, or unreachable, closes
// the proposal with that outcome and leaves the order's record alone.
func TestConfirmProposalOutcomes(t *testing.T) {
	for name, tc := range map[string]struct {
		answer *intents.RoadBookingConfirmationBody
		err    error
		status string
	}{
		"refused":     {&intents.RoadBookingConfirmationBody{BookingRef: "SR-BKG-0913", Status: intents.StatusRejected}, nil, ProposalRefused},
		"unreachable": {nil, errors.New("connection refused"), ProposalFailed},
	} {
		r, _, am := relayed(t)
		_ = r.Event(shipLeg, event("EV-1", "est"))
		_ = r.Alert(shipLeg, delay("AL-1", "EV-1", "2026-10-23T14:00:00Z", "2026-10-26T14:00:00Z"))
		am.answer, am.err = tc.answer, tc.err
		p, _ := r.Confirm(context.Background(), r.Proposals("MF-0007")[0].ID)
		b, _ := r.Orders.Bookings.Get(meridian, "MF-0007")
		if p.Status != tc.status || p.Outcome == "" || b.Order.Legs[1].Departure != "2026-10-25T20:00:00Z" {
			t.Errorf("%s: %+v", name, p)
		}
	}
	// Confirmed, but the order's legs changed meanwhile: said in the outcome.
	r, _, am := relayed(t)
	_ = r.Event(shipLeg, event("EV-1", "est"))
	_ = r.Alert(shipLeg, delay("AL-1", "EV-1", "2026-10-23T14:00:00Z", "2026-10-26T14:00:00Z"))
	_, _ = r.Orders.Bookings.Update(meridian, "MF-0007", func(b *store.Booking) error { b.Order.Legs = b.Order.Legs[:1]; return nil })
	am.answer = &intents.RoadBookingConfirmationBody{BookingRef: "SR-BKG-0913", Status: intents.StatusConfirmed,
		PlannedPickup: "2026-10-27T20:00:00Z", PlannedDelivery: "2026-10-29T02:00:00Z"}
	if p, _ := r.Confirm(context.Background(), r.Proposals("MF-0007")[0].ID); !strings.Contains(p.Outcome, "not recorded") {
		t.Errorf("legs changed: %+v", p)
	}
}

// TestControlProposals: the proposals, and confirming one, through the
// control API.
func TestControlProposals(t *testing.T) {
	r, _, am := relayed(t)
	_ = r.Event(shipLeg, event("EV-1", "est"))
	_ = r.Alert(shipLeg, delay("AL-1", "EV-1", "2026-10-23T14:00:00Z", "2026-10-26T14:00:00Z"))
	id := r.Proposals("MF-0007")[0].ID
	mux := http.NewServeMux()
	RegisterControl(mux, r.Orders, nil, r)
	do := func(method, path string) int {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		return rec.Code
	}
	am.err = errors.New("connection refused")
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/orders/MF-0007/proposals", http.StatusOK},
		{http.MethodPost, "/proposals/MF-PR-999999/confirm", http.StatusNotFound},
		{http.MethodPost, "/proposals/" + id + "/confirm", http.StatusBadGateway},
		{http.MethodPost, "/proposals/" + id + "/confirm", http.StatusConflict},
	} {
		if code := do(tc.method, tc.path); code != tc.want {
			t.Errorf("%s %s: HTTP %d, want %d", tc.method, tc.path, code, tc.want)
		}
	}
}
