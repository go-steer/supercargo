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
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/directory"
	"github.com/go-steer/supercargo/internal/llm"
	"github.com/go-steer/supercargo/internal/shipper"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/intents"
)

// truckNode books trucks and hands grants over, recording each.
type truckNode struct {
	node
	trucks  []shipper.RoadBookRequest
	grants  []shipper.GrantRequest
	failing error
}

func (n *truckNode) BookRoad(_ context.Context, carrier string, req intents.RoadBookingRequestBody) (*intents.RoadBookingConfirmationBody, error) {
	n.trucks = append(n.trucks, shipper.RoadBookRequest{Carrier: carrier, Request: req})
	if n.failing != nil {
		return nil, n.failing
	}
	return &intents.RoadBookingConfirmationBody{BookingRef: "SR-BKG-0913", Status: intents.StatusConfirmed,
		PlannedPickup: "2026-10-20T09:00:00Z", PlannedDelivery: "2026-10-21T15:00:00Z"}, nil
}

func (n *truckNode) Grant(_ context.Context, _, _ string, req shipper.GrantRequest) (*store.IssuedGrant, error) {
	n.grants = append(n.grants, req)
	if n.failing != nil {
		return nil, n.failing
	}
	return &store.IssuedGrant{Grantee: req.Grantee, Scope: req.Scope, Expires: time.Now().Add(7 * 24 * time.Hour)}, nil
}

var farmPlaces = []directory.Place{
	{ID: "farm", Name: "Fazenda Boa Safra", Party: intents.Party{Name: "Vale Verde", Domain: "vale-verde.example"},
		Place: intents.Place{Type: intents.PlaceDoor, UNLocationCode: "BRCGB", PlusCode: "58V75885+8J",
			Address: &intents.Address{Street: "Estrada MT-010, km 42", Locality: "Feliz Natal", Region: "MT", Country: "BR"}}},
	{ID: "dc", Name: "Mercado Aurora DC", Party: intents.Party{Name: "Mercado Aurora", Domain: "mercado-aurora.example"},
		Place: intents.Place{Type: intents.PlaceDoor, Facility: "Mercado Aurora DC, dock 3", UNLocationCode: "BRGRU",
			Address: &intents.Address{Street: "Rodovia Presidente Dutra, km 225", Locality: "Guarulhos", Region: "SP", Country: "BR"}}},
}

func truckAssistant(n *truckNode) (*Assistant, *Conversation) {
	a := &Assistant{Node: n, Drafts: &Drafts{}, Own: true, Places: farmPlaces, Now: func() time.Time { return thursday },
		Self: Self{Domain: "vale-verde.example", Name: "Vale Verde", Endpoint: "https://vale-verde.example/ulp"}}
	c := &Conversation{}
	c.userTexts = []string{"Book Serra (serra-transportes.example) and share it with mercado-aurora.example"}
	return a, c
}

const truckInput = `{"carrier":"serra-transportes.example","from":"farm","to":"dc","shipperRef":"VV-FTL-1","containerType":"40RF","grossWeightKg":26000,` +
	`"cargoDescription":"Chilled beef","hsCode":"0201","pickupFrom":"2026-10-20T06:00","deliverBy":"2026-10-22T18:00"}`

// TestTruckload: the truckload goes between our places, the farm by its
// Plus Code; only the dialog's send books it.
func TestTruckload(t *testing.T) {
	n := &truckNode{}
	a, c := truckAssistant(n)
	out, err := a.run(context.Background(), c, &turnState{}, llm.ToolCall{Name: ToolDraftRoad, Input: json.RawMessage(truckInput)})
	if err != nil {
		t.Fatal(err)
	}
	dr := out.(DraftResult)
	q := dr.Road
	if q == nil || !strings.HasPrefix(dr.DraftID, "TD-") || q.Pickup.PlusCode != "58V75885+8J" || q.Pickup.Facility != "Fazenda Boa Safra" ||
		q.Delivery.UNLocationCode != "BRGRU" || q.Consignee.Name != "Mercado Aurora" || q.Pickup.Earliest != "2026-10-20T06:00:00Z" ||
		q.Subscription.Endpoint != "https://vale-verde.example/ulp" || len(n.trucks) != 0 {
		t.Fatalf("%+v", dr)
	}
	sent, err := a.Send(context.Background(), dr.DraftID)
	if err != nil || sent.Trucked.BookingRef != "SR-BKG-0913" || len(n.trucks) != 1 || n.trucks[0].Carrier != "serra-transportes.example" {
		t.Fatalf("%+v %v", sent, err)
	}
	a.Model = FakeModel(func() time.Time { return thursday })
	if _, err := a.Report(context.Background(), c, sent); err != nil {
		t.Error(err)
	}
}

// TestTruckloadRefused: a carrier not named, ourselves, an unknown place,
// the same place twice, bad times, a private date or note: nothing.
func TestTruckloadRefused(t *testing.T) {
	a, c := truckAssistant(&truckNode{})
	a.Private = []string{"2026-10-25"}
	for name, edit := range map[string][2]string{
		"a carrier not named":       {`"carrier":"serra-transportes.example"`, `"carrier":"other.example"`},
		"ourselves":                 {`"carrier":"serra-transportes.example"`, `"carrier":"vale-verde.example"`},
		"an unknown place":          {`"from":"farm"`, `"from":"barn"`},
		"the same place":            {`"to":"dc"`, `"to":"farm"`},
		"a bad container":           {`"containerType":"40RF"`, `"containerType":"53HC"`},
		"a bad time":                {`"pickupFrom":"2026-10-20T06:00"`, `"pickupFrom":"tomorrow"`},
		"backwards":                 {`"deliverBy":"2026-10-22T18:00"`, `"deliverBy":"2026-10-19T18:00"`},
		"a private date, cargo":     {`"cargoDescription":"Chilled beef"`, `"cargoDescription":"Chilled beef for 2026-10-25"`},
		"a day from a private date": {`"deliverBy":"2026-10-22T18:00"`, `"deliverBy":"2026-10-24T22:00"`},
		"a private note in the ref": {`"shipperRef":"VV-FTL-1"`, `"shipperRef":"VV-2026-10-25"`},
		"an invalid body":           {`"hsCode":"0201"`, `"hsCode":"x"`},
	} {
		in := strings.Replace(truckInput, edit[0], edit[1], 1)
		if _, err := a.run(context.Background(), c, &turnState{}, llm.ToolCall{Name: ToolDraftRoad, Input: json.RawMessage(in)}); err == nil {
			t.Errorf("%s: drafted", name)
		}
	}
	if _, err := a.draftRoad(context.Background(), c, json.RawMessage(`[`)); err == nil {
		t.Error("not JSON")
	}
}

// TestShare: our booking is shared with a company the user named, as a
// delegation; only the dialog's send grants it.
func TestShare(t *testing.T) {
	n := &truckNode{node: node{shipments: []store.Summary{
		{Operator: "serra-transportes.example", Shipper: "vale-verde.example", Ref: "SR-BKG-0913", Kind: store.KindRoad},
		{Operator: "serra-transportes.example", Shipper: "someone.example", Ref: "SR-BKG-0001", Kind: store.KindRoad},
		{Operator: "meridian.example", Shipper: "vale-verde.example", Ref: "MF-0007", Kind: store.KindOrder},
	}}}
	a, c := truckAssistant(n)
	in := `{"bookingRef":"SR-BKG-0913","grantee":"mercado-aurora.example","days":7,"note":"Your DC's delivery"}`
	out, err := a.run(context.Background(), c, &turnState{}, llm.ToolCall{Name: ToolDraftGrant, Input: json.RawMessage(in)})
	if err != nil {
		t.Fatal(err)
	}
	dr := out.(DraftResult)
	if dr.Grant == nil || !strings.HasPrefix(dr.DraftID, "GD-") || dr.Grant.Carrier != "serra-transportes.example" || len(dr.Grant.Scope) != 2 || len(n.grants) != 0 {
		t.Fatalf("%+v", dr)
	}
	sent, err := a.Send(context.Background(), dr.DraftID)
	if err != nil || sent.Granted == nil || len(n.grants) != 1 || n.grants[0].Days != 7 {
		t.Fatalf("%+v %v", sent, err)
	}
	a.Model = FakeModel(func() time.Time { return thursday })
	if _, err := a.Report(context.Background(), c, sent); err != nil {
		t.Error(err)
	}
	a.Private = []string{"multa"}
	for name, raw := range map[string]string{
		"not named":       `{"bookingRef":"SR-BKG-0913","grantee":"other.example","days":7}`,
		"ourselves":       `{"bookingRef":"SR-BKG-0913","grantee":"vale-verde.example","days":7}`,
		"too long":        `{"bookingRef":"SR-BKG-0913","grantee":"mercado-aurora.example","days":40}`,
		"not our booking": `{"bookingRef":"SR-BKG-0001","grantee":"mercado-aurora.example","days":7}`,
		"an order":        `{"bookingRef":"MF-0007","grantee":"mercado-aurora.example","days":7}`,
		"a private note":  `{"bookingRef":"SR-BKG-0913","grantee":"mercado-aurora.example","days":7,"note":"multa"}`,
		"not JSON":        `[`,
	} {
		if _, err := a.draftGrant(context.Background(), c, json.RawMessage(raw)); err == nil {
			t.Errorf("%s: drafted", name)
		}
	}
}

// TestTruckSendFails: refused by the node, the draft is back; an
// assistant that can't book or share says so.
func TestTruckSendFails(t *testing.T) {
	n := &truckNode{failing: &NodeError{Status: http.StatusBadRequest, Message: "no"}, node: node{shipments: []store.Summary{
		{Operator: "serra-transportes.example", Shipper: "vale-verde.example", Ref: "SR-BKG-0913", Kind: store.KindRoad}}}}
	a, c := truckAssistant(n)
	out, _ := a.run(context.Background(), c, &turnState{}, llm.ToolCall{Name: ToolDraftRoad, Input: json.RawMessage(truckInput)})
	id := out.(DraftResult).DraftID
	if _, err := a.Send(context.Background(), id); err == nil || len(a.Drafts.Open()) != 1 {
		t.Errorf("refused: %v", err)
	}
	g, _ := a.draftGrant(context.Background(), c, json.RawMessage(`{"bookingRef":"SR-BKG-0913","grantee":"mercado-aurora.example","days":7}`))
	if _, err := a.Send(context.Background(), g.(DraftResult).DraftID); err == nil {
		t.Error("a refused grant sent")
	}
	plain := &node{shipments: n.shipments}
	b, bc := truckAssistant(&truckNode{})
	b.Node = plain
	out, _ = b.run(context.Background(), bc, &turnState{}, llm.ToolCall{Name: ToolDraftRoad, Input: json.RawMessage(truckInput)})
	if _, err := b.Send(context.Background(), out.(DraftResult).DraftID); err == nil || !strings.Contains(err.Error(), "can't book trucks") {
		t.Errorf("no booker: %v", err)
	}
	g, _ = b.draftGrant(context.Background(), bc, json.RawMessage(`{"bookingRef":"SR-BKG-0913","grantee":"mercado-aurora.example","days":7}`))
	if _, err := b.Send(context.Background(), g.(DraftResult).DraftID); err == nil || !strings.Contains(err.Error(), "can't share") {
		t.Errorf("no granter: %v", err)
	}
	// An answer without a confirmation: unknown.
	var none nilConf
	b.Node = &none
	out, _ = b.run(context.Background(), bc, &turnState{}, llm.ToolCall{Name: ToolDraftRoad, Input: json.RawMessage(truckInput)})
	if _, err := b.Send(context.Background(), out.(DraftResult).DraftID); !errors.Is(err, ErrOutcomeUnknown) {
		t.Errorf("no confirmation: %v", err)
	}
}

type nilConf struct{ node }

func (*nilConf) BookRoad(context.Context, string, intents.RoadBookingRequestBody) (*intents.RoadBookingConfirmationBody, error) {
	return nil, nil
}
