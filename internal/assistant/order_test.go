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
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/business"
	"github.com/go-steer/supercargo/internal/directory"
	"github.com/go-steer/supercargo/internal/llm"
	"github.com/go-steer/supercargo/internal/shipper"
	"github.com/go-steer/supercargo/pkg/intents"
)

// orderNode is a node that takes orders.
type orderNode struct {
	*node
	got    []shipper.OrderRequest
	answer *intents.ForwardingConfirmationBody
	err    error
}

func (n *orderNode) Order(_ context.Context, req shipper.OrderRequest) (*shipper.OrderResponse, error) {
	n.got = append(n.got, req)
	if n.err != nil {
		return nil, n.err
	}
	return &shipper.OrderResponse{Answer: n.answer}, nil
}

const orderBiz = `shipments:
  - shipperRef: CY-PO-55120
    requiredBy: 2026-11-07
    receivingSlot: { date: 2026-11-06, window: "08:00-12:00", warehouse: "Brazil launch stock" }
    notes: "Brazil launch: the São Paulo stores open with this stock"
`

var (
	tilburg = directory.Place{ID: "tilburg-warehouse", Name: "Tilburg warehouse",
		Party: intents.Party{Name: "Cymbal Shoes B.V.", Identifiers: []intents.Identifier{{Scheme: "EORI", Value: "NL123456789"}}},
		Place: intents.Place{Type: intents.PlaceDoor, UNLocationCode: "NLTLB", Address: &intents.Address{Street: "Ringbaan Oost 100", Locality: "Tilburg", Country: "NL"}}}
	cajamar = directory.Place{ID: "cajamar-dc", Name: "Cajamar DC", Party: intents.Party{Name: "Cymbal Calçados Ltda"},
		Place: intents.Place{Type: intents.PlaceDoor, Facility: "Cymbal Calçados DC, dock 2", UNLocationCode: "BRSAO",
			Address: &intents.Address{Street: "Rodovia Anhanguera, km 33", Locality: "Cajamar", Region: "SP", Country: "BR"}}}
	meridianEntry = directory.Entry{Domain: "meridian-forwarding.example", Name: "Meridian Forwarding", Roles: []string{directory.RoleForwarder}}
)

// orderingAssistant is Cymbal's own assistant, with its places, its
// forwarder and its private context.
func orderingAssistant(t *testing.T, n Node) *Assistant {
	t.Helper()
	biz, err := business.Parse([]byte(orderBiz))
	if err != nil {
		t.Fatal(err)
	}
	a := &Assistant{Model: FakeModel(func() time.Time { return thursday }), Node: n, Drafts: &Drafts{}, Skills: BuiltinSkills(),
		Self: Self{Domain: "cymbal-shoes.example", Name: "Cymbal Shoes", Endpoint: "https://cymbal-shoes.example/ulp"},
		Now:  func() time.Time { return thursday }, AddressBook: []directory.Entry{meridianEntry},
		Places: []directory.Place{tilburg, cajamar}, Business: biz, Private: biz.Private()}
	return a
}

const orderSentence = "Ask Meridian Forwarding to ship one 45-footer of the Brazil launch sneakers from our Tilburg warehouse to the Cajamar DC, door to door."

// TestOrderScene: from one sentence, the assistant reads our private
// context and our places and drafts an order with slack before the
// deadline; nothing private is in it, and nothing is sent until the
// dialog sends it.
func TestOrderScene(t *testing.T) {
	n := &orderNode{node: &node{}, answer: &intents.ForwardingConfirmationBody{OrderRef: "MF-0007", Status: intents.StatusPending}}
	a := orderingAssistant(t, n)
	var c Conversation
	r, err := a.Turn(context.Background(), &c, orderSentence)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Drafts) != 1 || r.Drafts[0].Order == nil || len(n.got) != 0 {
		t.Fatalf("drafts %+v, sent %d", r.Drafts, len(n.got))
	}
	d := r.Drafts[0]
	o := d.Order
	if d.To != "meridian-forwarding.example" || o.ShipperRef != "CY-PO-55120" || o.Shipper.Name != "Cymbal Shoes B.V." || o.Consignee.Name != "Cymbal Calçados Ltda" ||
		o.PlaceOfReceipt.UNLocationCode != "NLTLB" || o.PlaceOfDelivery.Facility != "Cymbal Calçados DC, dock 2" ||
		o.PlaceOfReceipt.Earliest != "2026-10-02T06:00:00Z" || o.PlaceOfDelivery.Latest != "2026-11-05T12:00:00Z" ||
		o.Incoterms.Rule != "DAP" || o.Incoterms.NamedPlace != "Cajamar DC" || o.Equipment[0].ISOType != "L5G1" ||
		o.Subscription == nil || o.Subscription.Endpoint != "https://cymbal-shoes.example/ulp" {
		t.Errorf("order %+v", o)
	}
	if d.NotSent["requiredBy"] != "2026-11-07" || !strings.Contains(d.NotSent["receivingSlot"], "08:00-12:00") || d.NotSent["notes"] == "" {
		t.Errorf("not sent %v", d.NotSent)
	}
	body, _ := json.Marshal(o)
	for _, private := range []string{"2026-11-07", "08:00-12:00", "Brazil launch", "São Paulo stores"} {
		if strings.Contains(string(body), private) {
			t.Errorf("the order carries %q", private)
		}
	}
	if !strings.Contains(r.Text, "Draft OD-0001") || !strings.Contains(r.Text, "2026-11-07") {
		t.Errorf("reply %q", r.Text)
	}
	// The dialog sends it, as drafted.
	sent, err := a.Send(context.Background(), d.DraftID)
	if err != nil || sent.Ordered == nil || len(n.got) != 1 || n.got[0].Forwarder != d.To || n.got[0].Order.PlaceOfDelivery.Latest != o.PlaceOfDelivery.Latest {
		t.Fatalf("sent %+v %v, node got %+v", sent, err, n.got)
	}
	rep, err := a.Report(context.Background(), &c, sent)
	if err != nil || !strings.Contains(rep.Text, "MF-0007") {
		t.Errorf("report %q %v", rep.Text, err)
	}
	if _, err := a.Send(context.Background(), d.DraftID); !errors.Is(err, ErrNoDraft) {
		t.Errorf("sent twice: %v", err)
	}
}

// TestDraftOrderRefused: an order the code won't draft. Each refusal is
// the model's to fix, and no draft is made.
func TestDraftOrderRefused(t *testing.T) {
	a := orderingAssistant(t, &orderNode{node: &node{}})
	c := &Conversation{userTexts: []string{"order with meridian-forwarding.example"}}
	ok := map[string]any{"forwarder": "meridian-forwarding.example", "from": "tilburg-warehouse", "to": "cajamar-dc", "containerType": "45HC",
		"containerCount": 1, "grossWeightKg": 12000, "cargoDescription": "Footwear", "hsCode": "6404", "readyFrom": "2026-10-02", "deliverBy": "2026-11-05"}
	with := func(k string, v any) json.RawMessage {
		m := map[string]any{}
		for kk, vv := range ok {
			m[kk] = vv
		}
		m[k] = v
		b, _ := json.Marshal(m)
		return b
	}
	for name, raw := range map[string]json.RawMessage{
		"a forwarder not named": with("forwarder", "other-forwarder.example"),
		"us":                    with("forwarder", "cymbal-shoes.example"),
		"no such place":         with("to", "rotterdam"),
		"no such origin":        with("from", "nowhere"),
		"the same place":        with("to", "tilburg-warehouse"),
		"a bad date":            with("deliverBy", "5 November"),
		"delivery before ready": with("deliverBy", "2026-10-01"),
		"the private deadline":  with("deliverBy", "2026-11-07"),
		"a private ready date":  with("readyFrom", "2026-11-07"),
		"no such container":     with("containerType", "53HC"),
		"the deadline in words": with("cargoDescription", "Footwear for the Nov 7 launch"),
		"a note in the cargo":   with("cargoDescription", "Footwear. Brazil launch: the São Paulo stores open with this stock"),
		"the deadline as a ref": with("shipperRef", "LAUNCH-2026-11-07"),
		"the slot's date":       with("deliverBy", "2026-11-06"),
		"not an order":          with("cargoDescription", ""),
		"not even JSON":         json.RawMessage(`[`),
	} {
		_, err := a.draftOrder(context.Background(), c, raw)
		var tool errTool
		if !errors.As(err, &tool) {
			t.Errorf("%s: %v", name, err)
		}
	}
	c.userTexts = append(c.userTexts, "and cymbal-shoes.example")
	if _, err := a.draftOrder(context.Background(), c, with("forwarder", "cymbal-shoes.example")); err == nil {
		t.Error("an order to ourselves")
	}
	if open := a.Drafts.Open(); len(open) != 0 {
		t.Errorf("drafts made: %+v", open)
	}
	// FCA is named at the origin.
	r, err := a.draftOrder(context.Background(), c, with("incoterms", "FCA"))
	if err != nil || r.(DraftResult).Order.Incoterms.NamedPlace != "Tilburg warehouse" {
		t.Errorf("FCA: %+v %v", r, err)
	}
}

// TestOrderTools: only the company's own assistant has the business
// context and orders; an outside agent's and the carrier's don't.
func TestOrderTools(t *testing.T) {
	has := func(a *Assistant, name string) bool {
		return slices.ContainsFunc(a.tools(), func(x llm.Tool) bool { return x.Name == name })
	}
	own := orderingAssistant(t, &node{})
	outside := &Assistant{Node: &node{}, Drafts: &Drafts{}, Private: own.Private}
	carrier := &Assistant{Carrier: &carrierNode{}, Drafts: &Drafts{}, Business: own.Business, Places: own.Places}
	for _, c := range []struct {
		name string
		a    *Assistant
		want bool
	}{{"own", own, true}, {"outside", outside, false}, {"carrier", carrier, false}} {
		if has(c.a, ToolBusiness) != c.want || has(c.a, ToolDraftOrder) != c.want {
			t.Errorf("%s: business %v, order %v", c.name, has(c.a, ToolBusiness), has(c.a, ToolDraftOrder))
		}
	}
	if len(Tools) != len(outside.tools()) {
		t.Error("the shared tool list grew")
	}
	// The context itself, marked private; none without one.
	r, _ := own.businessContext()
	if b, _ := json.Marshal(r); !strings.Contains(string(b), `"private":true`) || !strings.Contains(string(b), "CY-PO-55120") {
		t.Errorf("context %s", b)
	}
	r, _ = outside.businessContext()
	if b, _ := json.Marshal(r); strings.Contains(string(b), "CY-PO") {
		t.Errorf("no context: %s", b)
	}
}

// TestSendOrderFails: a node that can't order, a refusal before sending,
// and an answer that went missing.
func TestSendOrderFails(t *testing.T) {
	draft := func(a *Assistant) string {
		c := &Conversation{userTexts: []string{"meridian-forwarding.example"}}
		raw, _ := json.Marshal(map[string]any{"forwarder": "meridian-forwarding.example", "from": "tilburg-warehouse", "to": "cajamar-dc", "containerType": "45HC",
			"containerCount": 1, "grossWeightKg": 12000, "cargoDescription": "Footwear", "hsCode": "6404", "readyFrom": "2026-10-02", "deliverBy": "2026-11-05"})
		r, err := a.draftOrder(context.Background(), c, raw)
		if err != nil {
			t.Fatal(err)
		}
		return r.(DraftResult).DraftID
	}
	// A node without orders: not sent, the draft kept.
	a := orderingAssistant(t, &node{})
	id := draft(a)
	if _, err := a.Send(context.Background(), id); err == nil || !strings.Contains(err.Error(), "can't send orders") {
		t.Errorf("no orders: %v", err)
	}
	if _, err := a.Drafts.Get(id); err != nil {
		t.Error("the draft is gone")
	}
	// Refused before sending (a 4xx): the draft kept.
	n := &orderNode{node: &node{}, err: &NodeError{Status: 422, Message: "the guard refused it"}}
	a = orderingAssistant(t, n)
	id = draft(a)
	if _, err := a.Send(context.Background(), id); err == nil || !strings.HasPrefix(err.Error(), "not sent") {
		t.Errorf("refused: %v", err)
	}
	if _, err := a.Drafts.Get(id); err != nil {
		t.Error("the draft is gone")
	}
	// Sent, but no answer: may or may not have reached it; the draft gone.
	n.err = nil
	if _, err := a.Send(context.Background(), id); !errors.Is(err, ErrOutcomeUnknown) {
		t.Errorf("no answer: %v", err)
	}
	if _, err := a.Drafts.Get(id); !errors.Is(err, ErrNoDraft) {
		t.Error("the draft is still open")
	}
}

// TestOrderToolsOverMCP: an outside agent can't call the business
// context or draft an order, even by name; nothing is drafted.
func TestOrderToolsOverMCP(t *testing.T) {
	own := orderingAssistant(t, &orderNode{node: &node{}})
	outside := &Assistant{Node: own.Node, Drafts: &Drafts{Prefix: "MD"}, Private: own.Private, Self: own.Self, AddressBook: own.AddressBook}
	e := NewExternal()
	for _, name := range []string{ToolBusiness, ToolDraftOrder} {
		out := outside.RunTool(context.Background(), e, name, json.RawMessage(`{}`))
		if !out.IsError || strings.Contains(out.Content, "CY-PO") {
			t.Errorf("%s over MCP: %+v", name, out)
		}
	}
	if len(outside.Drafts.Open()) != 0 {
		t.Error("an outside agent drafted an order")
	}
}

// TestFakeOrderTrigger: the fake model places an order only when asked to
// ship one, not when asked about one.
func TestFakeOrderTrigger(t *testing.T) {
	for s, want := range map[string]bool{
		orderSentence:                                 true,
		"Why is my door-to-door order late?":          false,
		"Where is the door-to-door shipment MF-0007?": false,
		"What does door to door mean?":                false,
	} {
		got := orderPattern.MatchString(s) && shipPattern.MatchString(s) && !whyPattern.MatchString(s)
		if got != want {
			t.Errorf("%q: order %v, want %v", s, got, want)
		}
	}
}
