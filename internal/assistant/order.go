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
	"fmt"
	"slices"
	"time"

	"github.com/go-steer/supercargo/internal/business"
	"github.com/go-steer/supercargo/internal/directory"
	"github.com/go-steer/supercargo/internal/llm"
	"github.com/go-steer/supercargo/internal/ports"
	"github.com/go-steer/supercargo/internal/shipper"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/intents"
)

// Door to door (docs/proposals/door-to-door-forwarder.md): the shipper's
// own assistant drafts a forwarding order between two of the company's
// places, by their IDs in its address book, so every address sent is the
// company's own. It reads the company's private business context to pick
// the dates, and keeps that context out of the order: a person sends the
// draft in its dialog, which lists what stays home.

// Tool names.
const (
	ToolDraftOrder = "draft_order"
	ToolBusiness   = "get_business_context"
)

// BusinessTool reads the company's private business context. It is the
// shipper's own assistant's only: never an outside agent's.
var BusinessTool = llm.Tool{Name: ToolBusiness,
	Description: "Read our private business context: each shipment's own deadline (requiredBy), receiving slot and notes, by our reference, and the company's notes. Private: use it to advise the user and to pick dates with slack, but never put any of it in a message to another company.",
	Schema:      json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)}

// DraftOrderTool prepares a door-to-door order for a person to send.
var DraftOrderTool = llm.Tool{Name: ToolDraftOrder,
	Description: "Prepare a door-to-door order draft for a forwarder: it plans the legs and books the carriers. Nothing is sent: the user reviews the draft and confirms it themselves. The places are ours, by their IDs (list_partners lists them); the dates are ours to choose: leave slack before any private deadline, which is never sent. Returns the draft ID and what the forwarder would receive.",
	Schema: json.RawMessage(`{"type":"object","properties":{` +
		`"forwarder":{"type":"string","maxLength":253,"description":"The forwarder's domain: as the user wrote it, or the domain of the forwarder the user named, from list_partners"},` +
		`"from":{"type":"string","maxLength":32,"description":"The place of receipt: a place ID from list_partners"},` +
		`"to":{"type":"string","maxLength":32,"description":"The place of delivery: a place ID from list_partners"},` +
		`"incoterms":{"type":"string","enum":["EXW","FCA","CPT","CIP","DAP","DPU","DDP","FAS","FOB","CFR","CIF"],"description":"Incoterms 2020 rule; DAP if the user didn't say"},` +
		`"shipperRef":{"type":"string","maxLength":64,"description":"Our reference (a PO number), if the user or our business context gives one"},` +
		`"containerType":{"type":"string","enum":["20GP","40GP","40HC","45HC","20RF","40RF"]},` +
		`"containerCount":{"type":"integer","minimum":1,"maximum":999},` +
		`"grossWeightKg":{"type":"number","minimum":1,"maximum":40000,"description":"Each container's gross weight in kg (estimate if the user didn't say)"},` +
		`"cargoDescription":{"type":"string","maxLength":512},` +
		`"hsCode":{"type":"string","maxLength":10,"description":"The HS code's first 4 to 10 digits"},` +
		`"readyFrom":{"type":"string","maxLength":10,"description":"YYYY-MM-DD: when the cargo is ready at the place of receipt"},` +
		`"deliverBy":{"type":"string","maxLength":10,"description":"YYYY-MM-DD: the latest delivery the forwarder plans to. Sent: never a private date itself"}` +
		`},"required":["forwarder","from","to","containerType","containerCount","grossWeightKg","cargoDescription","hsCode","readyFrom","deliverBy"],"additionalProperties":false}`)}

// OrderDraft is a door-to-door order the assistant prepared: Request is
// exactly what would be sent to Forwarder; NotSent is the private context
// that goes with it, shown in the dialog as staying here.
type OrderDraft struct {
	Forwarder string
	Request   intents.ForwardingRequestBody
	NotSent   map[string]string
}

// Orderer is a node that sends orders (the shipper's control API's POST
// /orders): only the confirmation dialog calls it.
type Orderer interface {
	Order(ctx context.Context, req shipper.OrderRequest) (*shipper.OrderResponse, error)
}

// Order sends a door-to-door order.
func (n ControlNode) Order(ctx context.Context, req shipper.OrderRequest) (*shipper.OrderResponse, error) {
	var out shipper.OrderResponse
	if err := n.do(ctx, "POST", "/orders", req, &out); err != nil {
		return nil, withMessage(err, out.Error)
	}
	return &out, nil
}

// orderInput is draft_order's input.
type orderInput struct {
	Forwarder        string  `json:"forwarder"`
	From             string  `json:"from"`
	To               string  `json:"to"`
	Incoterms        string  `json:"incoterms"`
	ShipperRef       string  `json:"shipperRef"`
	ContainerType    string  `json:"containerType"`
	ContainerCount   llm.Int `json:"containerCount"`
	GrossWeightKg    float64 `json:"grossWeightKg"`
	CargoDescription string  `json:"cargoDescription"`
	HSCode           string  `json:"hsCode"`
	ReadyFrom        string  `json:"readyFrom"`
	DeliverBy        string  `json:"deliverBy"`
}

// originNamed are the Incoterms rules named at the origin; the others are
// named at the destination.
var originNamed = []string{"EXW", "FCA", "FAS", "FOB"}

func (a *Assistant) draftOrder(ctx context.Context, c *Conversation, raw json.RawMessage) (any, error) {
	var in orderInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, refuse("%v", err)
	}
	if err := a.named(c, in.Forwarder); err != nil {
		return nil, err
	}
	if in.Forwarder == a.Self.Domain {
		return nil, refuse("%s is us; an order goes to a forwarder", in.Forwarder)
	}
	from, ok1 := a.place(in.From)
	to, ok2 := a.place(in.To)
	switch {
	case !ok1 || !ok2:
		return nil, refuse("no place %q; list_partners lists our places", map[bool]string{true: in.To, false: in.From}[ok1])
	case in.From == in.To:
		return nil, refuse("the order goes from %s to %s: two different places", in.From, in.To)
	}
	iso := ports.SizeType(in.ContainerType)
	if iso == "" {
		return nil, refuse("no container type %s", in.ContainerType)
	}
	ready, err1 := time.Parse(time.DateOnly, in.ReadyFrom)
	by, err2 := time.Parse(time.DateOnly, in.DeliverBy)
	switch {
	case err1 != nil || err2 != nil:
		return nil, refuse("readyFrom and deliverBy are YYYY-MM-DD dates")
	case !by.After(ready):
		return nil, refuse("deliverBy %s is not after readyFrom %s", in.DeliverBy, in.ReadyFrom)
	}
	// A private date (a deadline, a slot) never goes in an order, even as
	// its deliver-by: the forwarder would learn it. Nor does any private
	// date or note in its free text, however written ("Nov 7").
	for _, d := range []string{in.ReadyFrom, in.DeliverBy} {
		if slices.Contains(a.private(), d) {
			return nil, refuse("%s is a date in our private business context: the order must not carry it. Choose a date with slack, or ask the user", d)
		}
	}
	for field, text := range map[string]string{"cargoDescription": in.CargoDescription, "shipperRef": in.ShipperRef} {
		if p := a.privateIn(text); p != "" {
			return nil, refuse("%s mentions %s, which is private to us: the order must not carry it; leave it out", field, p)
		}
	}
	rule := in.Incoterms
	if rule == "" {
		rule = "DAP"
	}
	named := to.Name
	if slices.Contains(originNamed, rule) {
		named = from.Name
	}
	req := intents.ForwardingRequestBody{ShipperRef: in.ShipperRef, Incoterms: intents.Incoterms{Rule: rule, Version: "2020", NamedPlace: named},
		Shipper: from.Party, Consignee: to.Party, PlaceOfReceipt: from.Place, PlaceOfDelivery: to.Place,
		Equipment: []intents.Equipment{{ISOType: iso, Count: int(in.ContainerCount), GrossWeightKg: in.GrossWeightKg}},
		Cargo:     intents.Cargo{Description: fold(in.CargoDescription), HSCode: in.HSCode}}
	req.PlaceOfReceipt.Earliest = in.ReadyFrom + "T06:00:00Z"
	req.PlaceOfDelivery.Latest = in.DeliverBy + "T12:00:00Z"
	if a.Self.Endpoint != "" {
		req.Subscription = &intents.Subscription{Endpoint: a.Self.Endpoint, Intents: []string{intents.TnTEvent, intents.TnTAlert}}
	}
	probe := req
	if probe.ShipperRef == "" {
		probe.ShipperRef = "OD-0000" // filled in with the draft ID
	}
	if err := probe.Validate(); err != nil {
		return nil, refuse("the draft isn't a valid order: %v", err)
	}
	newPartner := !a.inAddressBook(in.Forwarder)
	if newPartner {
		if s, err := a.Node.Shipments(ctx); err == nil {
			newPartner = !slices.ContainsFunc(s, func(x store.Summary) bool { return x.Operator == in.Forwarder })
		}
	}
	dr, err := a.Drafts.add(Draft{Order: &OrderDraft{Forwarder: in.Forwarder, Request: req, NotSent: a.notSentFor(in.ShipperRef)},
		NewPartner: newPartner, Created: a.now()})
	if err != nil {
		return nil, refuse("%v", err)
	}
	return dr.Result(), nil
}

// place is one of our places, by its ID.
func (a *Assistant) place(id string) (directory.Place, bool) {
	i := slices.IndexFunc(a.Places, func(p directory.Place) bool { return p.ID == id })
	if i < 0 {
		return directory.Place{}, false
	}
	return a.Places[i], true
}

// notSentFor is the private context for our reference ref, as the dialog
// lists it: what stays here.
func (a *Assistant) notSentFor(ref string) map[string]string {
	if a.Business == nil || ref == "" {
		return nil
	}
	for _, s := range a.Business.Shipments {
		if s.ShipperRef != ref {
			continue
		}
		out := map[string]string{}
		if s.RequiredBy != "" {
			out["requiredBy"] = s.RequiredBy
		}
		if sl := s.ReceivingSlot; sl != nil {
			out["receivingSlot"] = fmt.Sprintf("%s %s, %s", sl.Date, sl.Window, sl.Warehouse)
		}
		if s.Notes != "" {
			out["notes"] = s.Notes
		}
		if len(out) == 0 {
			return nil
		}
		return out
	}
	return nil
}

// businessContext is get_business_context's result: the context as it
// is, marked private.
func (a *Assistant) businessContext() (any, error) {
	if a.Business == nil {
		return map[string]any{"private": true, "shipments": []business.Shipment{}, "notes": []string{}}, nil
	}
	return map[string]any{"private": true, "note": "Ours alone: never put any of this in a message to another company.",
		"shipments": a.Business.Shipments, "notes": a.Business.Notes}, nil
}

// sendOrder sends a drafted order through the node.
func (a *Assistant) sendOrder(ctx context.Context, d Draft) (*Sent, error) {
	o, ok := a.Node.(Orderer)
	if !ok {
		a.Drafts.Put(d)
		return nil, errors.New("not sent: this assistant can't send orders")
	}
	resp, err := o.Order(ctx, shipper.OrderRequest{Forwarder: d.Order.Forwarder, Order: d.Order.Request})
	if err != nil {
		return nil, a.notSent(d, err)
	}
	if resp.Answer == nil {
		return nil, fmt.Errorf("%w (the node answered without the forwarder's answer)", ErrOutcomeUnknown)
	}
	return &Sent{Draft: d, Ordered: resp.Answer}, nil
}
