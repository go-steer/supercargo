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

package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/backend"
	"github.com/go-steer/supercargo/internal/forwarder"
	"github.com/go-steer/supercargo/internal/llm"
	"github.com/go-steer/supercargo/pkg/intents"
)

func (d *desk) SetRate(_ context.Context, ref string, leg int, rate float64) (*forwarder.PlanResponse, error) {
	d.rates = append(d.rates, forwarder.RateRequest{Leg: leg, Rate: rate})
	if d.rateErr != nil {
		return nil, d.rateErr
	}
	steps := append([]forwarder.Step(nil), d.view.Steps...)
	f := *steps[leg-1].Freight
	f.Rate, f.Status, f.Shortfall = rate, forwarder.FreightOK, 0
	steps[leg-1].Freight = &f
	return &forwarder.PlanResponse{Steps: steps}, nil
}

// pendingOrder is MF-0007 waiting to be booked, Serra's truck R$ 90,01
// under the ANTT floor.
func pendingOrder() forwarder.OrderView {
	floor := &backend.Floor{DistanceKm: 117.6, Duration: 7697 * time.Second, TollTotal: 242, Freight: 1140.01, LoadUnload: 497.66,
		Tolls:      []backend.Toll{{Name: "Riacho Grande", Amount: 179.5}, {Name: "Rodoanel Oeste", Amount: 36.5}, {Name: "Perus", Amount: 26}},
		Resolution: "Resolução ANTT nº 6.034", Source: "QualP (simulated)"}
	return forwarder.OrderView{Ref: "MF-0007", Shipper: "cymbal-shoes.example", Answer: intents.ForwardingConfirmationBody{Status: intents.StatusPending},
		Steps: []forwarder.Step{
			{Mode: "road", Carrier: "polder-haulage.example", Name: "Polder Haulage", From: "NLTLB", To: "NLRTM", EDI: true},
			{Mode: "ocean", Carrier: "ocean-blue.example", Name: "Ocean Blue", From: "NLRTM", To: "BRSSZ"},
			{Mode: "road", Carrier: "serra-transportes.example", Name: "Serra Transportes", From: "BRSSZ", To: "BRSAO",
				Freight: &forwarder.LegFreight{Rate: 1050, Currency: "BRL", Axles: 5, LoadType: "conteineirizada", Table: "A",
					Route: []string{"Santos, SP", "Cajamar, SP"}, Floor: floor, Status: forwarder.FreightBelow, Shortfall: 90.01}},
		}}
}

// TestFreightScene: asked why a leg won't book, the desk's assistant
// checks its freight, drafts a rate above the floor and explains; only
// the dialog's send sets it, and nothing goes to anyone.
func TestFreightScene(t *testing.T) {
	d := &desk{view: pendingOrder()}
	a := deskAssistant(d)
	var c Conversation
	r, err := a.Turn(context.Background(), &c, "Why can't we book leg 3 of MF-0007?")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"below Brazil's legal minimum freight", "R$ 1050.00", "R$ 1140.01", "R$ 90.01 short", "twice the difference",
		"Vale-Pedágio", "QualP (simulated)", "Draft RD-0001", "R$ 1180.00"} {
		if !strings.Contains(r.Text, want) {
			t.Errorf("the reply lacks %q:\n%s", want, r.Text)
		}
	}
	if len(r.Drafts) != 1 || r.Drafts[0].Rate == nil || len(d.rates) != 0 {
		t.Fatalf("drafts %+v, rates %v", r.Drafts, d.rates)
	}
	rd := r.Drafts[0].Rate
	if rd.Rate != 1180 || rd.Was != 1050 || rd.Floor != 1140.01 || rd.TollTotal != 242 || rd.CarrierName != "Serra Transportes" || rd.Route != "Santos, SP → Cajamar, SP" {
		t.Errorf("rate draft %+v", rd)
	}
	sent, err := a.Send(context.Background(), r.Drafts[0].DraftID)
	if err != nil || sent.Rated != forwarder.FreightOK || len(d.rates) != 1 || d.rates[0] != (forwarder.RateRequest{Leg: 3, Rate: 1180}) {
		t.Fatalf("sent %+v %v, rates %v", sent, err, d.rates)
	}
	rep, err := a.Report(context.Background(), &c, sent)
	if err != nil || !strings.Contains(rep.Text, "Done: leg 3 of MF-0007 is now at R$ 1180.00") {
		t.Errorf("report %q %v", rep.Text, err)
	}
}

// TestCheckFreight: what check_freight says a leg's booking would do, and
// a leg or order it can't check.
func TestCheckFreight(t *testing.T) {
	ctx := context.Background()
	check := func(a *Assistant, raw string) (freightView, error) {
		out, err := a.runCarrier(ctx, llm.ToolCall{Name: ToolCheckFreight, Input: json.RawMessage(raw)})
		if err != nil {
			return freightView{}, err
		}
		return out.(freightView), nil
	}
	v := pendingOrder()
	a := deskAssistant(&desk{view: v})
	got, err := check(a, `{"orderRef":"MF-0007","leg":3}`)
	if err != nil || got.BookingNow != "refused until the rate is at or above the floor" || got.DriveHours != 2.1 || got.Freight.Floor.Freight != 1140.01 {
		t.Fatalf("%+v %v", got, err)
	}
	for status, want := range map[string]string{forwarder.FreightOK: "may be booked", forwarder.FreightUnchecked: "couldn't be checked"} {
		v := pendingOrder()
		v.Steps[2].Freight.Status = status
		if got, _ := check(deskAssistant(&desk{view: v}), `{"orderRef":"MF-0007","leg":3}`); !strings.Contains(got.BookingNow, want) {
			t.Errorf("%s: %q", status, got.BookingNow)
		}
	}
	v = pendingOrder()
	v.Steps[2].Booked = "SR-BKG-0913"
	if got, _ := check(deskAssistant(&desk{view: v}), `{"orderRef":"MF-0007","leg":3}`); got.BookingNow != "booked already" {
		t.Errorf("booked: %q", got.BookingNow)
	}
	confirmed := pendingOrder()
	confirmed.Answer.Status = intents.StatusConfirmed
	for name, tc := range map[string]struct {
		a   *Assistant
		raw string
	}{
		"no freight on the ship": {a, `{"orderRef":"MF-0007","leg":2}`},
		"no leg 9":               {a, `{"orderRef":"MF-0007","leg":9}`},
		"no such order":          {a, `{"orderRef":"MF-9999","leg":3}`},
		"a confirmed order":      {deskAssistant(&desk{view: confirmed}), `{"orderRef":"MF-0007","leg":3}`},
	} {
		if _, err := check(tc.a, tc.raw); err == nil {
			t.Errorf("%s: checked", name)
		}
	}
}

// TestDraftRateRefused: below the floor, far above it, a booked leg, a
// leg whose floor couldn't be had: nothing drafted.
func TestDraftRateRefused(t *testing.T) {
	booked, unchecked := pendingOrder(), pendingOrder()
	booked.Steps[2].Booked = "SR-BKG-0913"
	unchecked.Steps[2].Freight.Floor, unchecked.Steps[2].Freight.Status, unchecked.Steps[2].Freight.Error = nil, forwarder.FreightUnchecked, "qualp: HTTP 503"
	for name, tc := range map[string]struct {
		view forwarder.OrderView
		raw  string
	}{
		"below the floor":  {pendingOrder(), `{"orderRef":"MF-0007","leg":3,"rate":1100}`},
		"over three times": {pendingOrder(), `{"orderRef":"MF-0007","leg":3,"rate":4000}`},
		"booked":           {booked, `{"orderRef":"MF-0007","leg":3,"rate":1200}`},
		"unchecked":        {unchecked, `{"orderRef":"MF-0007","leg":3,"rate":1200}`},
		"the ship":         {pendingOrder(), `{"orderRef":"MF-0007","leg":2,"rate":1200}`},
		"not JSON":         {pendingOrder(), `[`},
	} {
		a := deskAssistant(&desk{view: tc.view})
		if _, err := a.runCarrier(context.Background(), llm.ToolCall{Name: ToolDraftRate, Input: json.RawMessage(tc.raw)}); err == nil {
			t.Errorf("%s: drafted", name)
		}
		if len(a.Drafts.Open()) != 0 {
			t.Errorf("%s: a draft was made", name)
		}
	}
}

// TestSendRateFails: refused by the node puts the draft back; an
// assistant without the desk can't set it.
func TestSendRateFails(t *testing.T) {
	d := &desk{view: pendingOrder(), rateErr: &NodeError{Status: 409, Message: "answered"}}
	a := deskAssistant(d)
	r, err := a.draftRate(context.Background(), json.RawMessage(`{"orderRef":"MF-0007","leg":3,"rate":1180}`))
	if err != nil {
		t.Fatal(err)
	}
	id := r.(DraftResult).DraftID
	if _, err := a.Send(context.Background(), id); err == nil || !strings.HasPrefix(err.Error(), "not sent") {
		t.Errorf("refused: %v", err)
	}
	if len(a.Drafts.Open()) != 1 {
		t.Error("the draft wasn't put back")
	}
	a.Desk = nil
	if _, err := a.Send(context.Background(), id); err == nil || !strings.Contains(err.Error(), "can't set rates") {
		t.Errorf("no desk: %v", err)
	}
	// The fake's other answers: a leg at or above the floor, unchecked.
	ok, unchecked := pendingOrder(), pendingOrder()
	ok.Steps[2].Freight.Status = forwarder.FreightOK
	unchecked.Steps[2].Freight.Floor, unchecked.Steps[2].Freight.Status, unchecked.Steps[2].Freight.Error = nil, forwarder.FreightUnchecked, "qualp: HTTP 503"
	for want, v := range map[string]forwarder.OrderView{"is fine": ok, "can't be checked": unchecked} {
		var c Conversation
		r, err := deskAssistant(&desk{view: v}).Turn(context.Background(), &c, "What's the freight on leg 3 of MF-0007?")
		if err != nil || !strings.Contains(r.Text, want) {
			t.Errorf("%s: %q %v", want, r.Text, err)
		}
	}
	for in, want := range map[string]string{
		"x": "couldn't read",
		`{"order":"MF-0007","leg":3,"rate":900,"freightStatus":"below"}`: "check it again before booking",
	} {
		if got := rateReport(in).Text; !strings.Contains(got, want) {
			t.Errorf("rateReport(%s) = %q", in, got)
		}
	}
	if got := freightStep("c", "MF-0007", "leg 3", []step{{result: llm.ToolResult{Content: "["}}}).Text; !strings.Contains(got, "couldn't read") {
		t.Errorf("an unreadable check: %q", got)
	}
	if !errors.Is(error(nil), nil) {
		t.Fatal()
	}
}
