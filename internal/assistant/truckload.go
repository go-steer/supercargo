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
	"net/url"
	"slices"
	"time"

	"github.com/go-steer/supercargo/internal/llm"
	"github.com/go-steer/supercargo/internal/ports"
	"github.com/go-steer/supercargo/internal/shipper"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/delegation"
	"github.com/go-steer/supercargo/pkg/intents"
)

// A truckload booked directly with a road carrier, between two of our
// places (a farm's loading point by its Plus Code, a customer's DC), and
// a booking's tracking shared with another company by delegation
// (milestone 7). The company's own assistant drafts both; a person sends
// each from its dialog.

// The truckload tools' names.
const (
	ToolDraftRoad  = "draft_road_booking"
	ToolDraftGrant = "draft_grant"
)

// DraftRoadTool prepares a truckload for a person to send.
var DraftRoadTool = llm.Tool{Name: ToolDraftRoad,
	Description: "Prepare a truckload booking draft for a road carrier: one truck from one of our places to another, by their IDs (list_partners lists them; a place may be pinned by its Plus Code, which goes with it). Nothing is sent: the user reviews and confirms it themselves. Returns the draft ID and what the carrier would receive.",
	Schema: json.RawMessage(`{"type":"object","properties":{` +
		`"carrier":{"type":"string","maxLength":253,"description":"The road carrier's domain, as the user named it or from list_partners"},` +
		`"from":{"type":"string","maxLength":32,"description":"The pickup: a place ID from list_partners"},` +
		`"to":{"type":"string","maxLength":32,"description":"The delivery: a place ID from list_partners"},` +
		`"shipperRef":{"type":"string","maxLength":64,"description":"Our reference, if the user or our business context gives one"},` +
		`"containerType":{"type":"string","enum":["20GP","40GP","40HC","45HC","20RF","40RF"],"description":"40RF for a refrigerated load"},` +
		`"grossWeightKg":{"type":"number","minimum":1,"maximum":40000},` +
		`"cargoDescription":{"type":"string","maxLength":512},` +
		`"hsCode":{"type":"string","maxLength":10},` +
		`"pickupFrom":{"type":"string","maxLength":20,"description":"YYYY-MM-DDTHH:MM, UTC: the earliest pickup"},` +
		`"deliverBy":{"type":"string","maxLength":20,"description":"YYYY-MM-DDTHH:MM, UTC: the latest delivery (sent: never a private date itself)"}` +
		`},"required":["carrier","from","to","containerType","grossWeightKg","cargoDescription","hsCode","pickupFrom","deliverBy"],"additionalProperties":false}`)}

// DraftGrantTool prepares sharing a booking's tracking with another
// company: a delegation, signed by our node when the user confirms.
var DraftGrantTool = llm.Tool{Name: ToolDraftGrant,
	Description: "Prepare sharing one of our bookings with another company the user named (its receiving DC, say): a delegation letting it follow the booking's signed events (subscribe) and ask its carrier about it (read), for 1-31 days. Nothing is granted until the user confirms it themselves.",
	Schema: json.RawMessage(`{"type":"object","properties":{` +
		`"bookingRef":{"type":"string","maxLength":64},` +
		`"carrier":{"type":"string","maxLength":253,"description":"The booking's carrier, if two carriers' bookings share the reference"},` +
		`"grantee":{"type":"string","maxLength":253,"description":"The company's domain, as the user named it"},` +
		`"days":{"type":"integer","minimum":1,"maximum":31},` +
		`"note":{"type":"string","maxLength":280,"description":"A short note to the company, from us"}` +
		`},"required":["bookingRef","grantee","days"],"additionalProperties":false}`)}

// RoadDraft is a truckload booking, prepared: Request is exactly what
// would go to Carrier.
type RoadDraft struct {
	Carrier string                         `json:"carrier"`
	Request intents.RoadBookingRequestBody `json:"request"`
	NotSent map[string]string              `json:"notSent,omitempty"`
}

// GrantDraft is a delegation, prepared.
type GrantDraft struct {
	Carrier    string   `json:"carrier"`
	BookingRef string   `json:"bookingRef"`
	Grantee    string   `json:"grantee"`
	Scope      []string `json:"scope"`
	Days       int      `json:"days"`
	Note       string   `json:"note,omitempty"`
}

// RoadBooker sends a truckload (the shipper's control API's POST
// /book/road): only the dialog calls it.
type RoadBooker interface {
	BookRoad(ctx context.Context, carrier string, req intents.RoadBookingRequestBody) (*intents.RoadBookingConfirmationBody, error)
}

// Granter hands a delegation over (POST /shipments/{op}/{ref}/grant): only
// the dialog calls it.
type Granter interface {
	Grant(ctx context.Context, operator, ref string, req shipper.GrantRequest) (*store.IssuedGrant, error)
}

// BookRoad implements RoadBooker.
func (n ControlNode) BookRoad(ctx context.Context, carrier string, req intents.RoadBookingRequestBody) (*intents.RoadBookingConfirmationBody, error) {
	var out shipper.RoadBookResponse
	if err := n.do(ctx, "POST", "/book/road", shipper.RoadBookRequest{Carrier: carrier, Request: req}, &out); err != nil {
		return nil, withMessage(err, out.Error)
	}
	return out.Confirmation, nil
}

// Grant implements Granter.
func (n ControlNode) Grant(ctx context.Context, operator, ref string, req shipper.GrantRequest) (*store.IssuedGrant, error) {
	var out shipper.GrantResponse
	if err := n.do(ctx, "POST", "/shipments/"+url.PathEscape(operator)+"/"+url.PathEscape(ref)+"/grant", req, &out); err != nil {
		return nil, withMessage(err, out.Error)
	}
	return out.Grant, nil
}

// roadLayout is the draft's times: a minute's precision.
const roadLayout = "2006-01-02T15:04"

// privateDay is the private date (the business file's, or an open
// draft's required-by) on or a day either side of any of ts, or "": a
// UTC time's date may be the day before or after where the date is
// local.
func (a *Assistant) privateDay(ts ...time.Time) string {
	private := append(a.private(), a.Drafts.requiredBy()...)
	for _, t := range ts {
		for _, d := range []time.Time{t.AddDate(0, 0, -1), t, t.AddDate(0, 0, 1)} {
			if s := d.Format(time.DateOnly); slices.Contains(private, s) {
				return s
			}
		}
	}
	return ""
}

func (a *Assistant) draftRoad(ctx context.Context, c *Conversation, raw json.RawMessage) (any, error) {
	var in struct {
		Carrier, From, To, ShipperRef, ContainerType, CargoDescription, HSCode, PickupFrom, DeliverBy string
		GrossWeightKg                                                                                 float64
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, refuse("%v", err)
	}
	if err := a.named(c, in.Carrier); err != nil {
		return nil, err
	}
	if in.Carrier == a.Self.Domain {
		return nil, refuse("%s is us; a truckload goes to a road carrier", in.Carrier)
	}
	from, ok1 := a.place(in.From)
	to, ok2 := a.place(in.To)
	switch {
	case !ok1 || !ok2:
		return nil, refuse("no place %q; list_partners lists our places", map[bool]string{true: in.To, false: in.From}[ok1])
	case in.From == in.To:
		return nil, refuse("the truck goes from %s to %s: two different places", in.From, in.To)
	}
	iso := ports.SizeType(in.ContainerType)
	if iso == "" {
		return nil, refuse("no container type %s", in.ContainerType)
	}
	pickup, err1 := time.Parse(roadLayout, in.PickupFrom)
	by, err2 := time.Parse(roadLayout, in.DeliverBy)
	switch {
	case err1 != nil || err2 != nil:
		return nil, refuse("pickupFrom and deliverBy are YYYY-MM-DDTHH:MM, UTC")
	case !by.After(pickup):
		return nil, refuse("deliverBy %s is not after pickupFrom %s", in.DeliverBy, in.PickupFrom)
	}
	if d := a.privateDay(pickup, by); d != "" {
		return nil, refuse("%s is a date in our private business context: the booking must not carry it. Choose a date with slack, or ask the user", d)
	}
	for field, text := range map[string]string{"cargoDescription": in.CargoDescription, "shipperRef": in.ShipperRef} {
		if p := a.privateIn(text); p != "" {
			return nil, refuse("%s mentions %s, which is private to us: the booking must not carry it; leave it out", field, p)
		}
	}
	stop := func(p intents.Place, name string) intents.Stop {
		facility := p.Facility
		if facility == "" {
			facility = name
		}
		return intents.Stop{Facility: facility, FacilityCode: p.FacilityCode, UNLocationCode: p.UNLocationCode, Address: p.Address, PlusCode: p.PlusCode}
	}
	req := intents.RoadBookingRequestBody{ShipperRef: in.ShipperRef, Shipper: from.Party, Pickup: stop(from.Place, from.Name), Delivery: stop(to.Place, to.Name),
		Equipment: []intents.Equipment{{ISOType: iso, Count: 1, GrossWeightKg: in.GrossWeightKg}},
		Cargo:     intents.Cargo{Description: fold(in.CargoDescription), HSCode: in.HSCode}}
	if to.Party.Name != "" {
		cons := to.Party
		req.Consignee = &cons
	}
	req.Shipper.Domain = a.Self.Domain
	req.Pickup.Earliest = pickup.UTC().Format("2006-01-02T15:04:05Z")
	req.Delivery.Latest = by.UTC().Format("2006-01-02T15:04:05Z")
	if a.Self.Endpoint != "" {
		req.Subscription = &intents.Subscription{Endpoint: a.Self.Endpoint, Intents: []string{intents.TnTEvent, intents.TnTAlert}}
	}
	probe := req
	if probe.ShipperRef == "" {
		probe.ShipperRef = "TD-0000" // filled in with the draft ID
	}
	if err := probe.Validate(); err != nil {
		return nil, refuse("the draft isn't a valid road booking: %v", err)
	}
	newPartner := !a.inAddressBook(in.Carrier)
	if newPartner {
		if s, err := a.Node.Shipments(ctx); err == nil {
			newPartner = !slices.ContainsFunc(s, func(x store.Summary) bool { return x.Operator == in.Carrier })
		}
	}
	dr, err := a.Drafts.add(Draft{Road: &RoadDraft{Carrier: in.Carrier, Request: req, NotSent: a.notSentFor(in.ShipperRef)}, NewPartner: newPartner, Created: a.now()})
	if err != nil {
		return nil, refuse("%v", err)
	}
	return dr.Result(), nil
}

func (a *Assistant) draftGrant(ctx context.Context, c *Conversation, raw json.RawMessage) (any, error) {
	var in struct {
		BookingRef, Carrier, Grantee, Note string
		Days                               int
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, refuse("%v", err)
	}
	if err := a.named(c, in.Grantee); err != nil {
		return nil, err
	}
	if in.Grantee == a.Self.Domain {
		return nil, refuse("%s is us", in.Grantee)
	}
	if in.Days < 1 || in.Days > shipper.MaxGrantDays {
		return nil, refuse("days is 1-%d", shipper.MaxGrantDays)
	}
	if p := a.privateIn(in.Note); p != "" {
		return nil, refuse("the note mentions %s, which is private to us: leave it out", p)
	}
	all, err := a.Node.Shipments(ctx)
	if err != nil {
		return nil, err
	}
	var found []store.Summary
	for _, s := range all {
		if s.Ref == in.BookingRef && s.Shipper == a.Self.Domain && s.DelegatedBy == "" && (in.Carrier == "" || s.Operator == in.Carrier) {
			found = append(found, s)
		}
	}
	switch {
	case len(found) == 0:
		return nil, refuse("we hold no booking %s as its shipper: only our own bookings can be shared", in.BookingRef)
	case len(found) > 1:
		return nil, refuse("%s is a booking with more than one carrier: say which (carrier)", in.BookingRef)
	case found[0].Kind == store.KindOrder:
		return nil, refuse("%s is a door-to-door order: share one of its legs' bookings instead", in.BookingRef)
	case found[0].Status == intents.StatusCancelled:
		return nil, refuse("%s was cancelled: there's nothing to follow", in.BookingRef)
	}
	i := slices.IndexFunc(all, func(s store.Summary) bool { return s.Ref == found[0].Ref && s.Operator == found[0].Operator })
	gd := &GrantDraft{Carrier: all[i].Operator, BookingRef: in.BookingRef, Grantee: in.Grantee, Scope: delegation.Scopes(), Days: in.Days, Note: fold(in.Note)}
	dr, err := a.Drafts.add(Draft{Grant: gd, NewPartner: !a.inAddressBook(in.Grantee), Created: a.now()})
	if err != nil {
		return nil, refuse("%v", err)
	}
	return dr.Result(), nil
}

// sendRoad books a drafted truckload through the node.
func (a *Assistant) sendRoad(ctx context.Context, d Draft) (*Sent, error) {
	rb, ok := a.Node.(RoadBooker)
	if !ok {
		a.Drafts.Put(d)
		return nil, errors.New("not sent: this assistant can't book trucks")
	}
	conf, err := rb.BookRoad(ctx, d.Road.Carrier, d.Road.Request)
	if err != nil {
		return nil, a.notSent(d, err)
	}
	if conf == nil {
		return nil, fmt.Errorf("%w (the node answered without the carrier's answer)", ErrOutcomeUnknown)
	}
	return &Sent{Draft: d, Trucked: conf}, nil
}

// sendGrant hands a drafted delegation over through the node.
func (a *Assistant) sendGrant(ctx context.Context, d Draft) (*Sent, error) {
	g, ok := a.Node.(Granter)
	if !ok {
		a.Drafts.Put(d)
		return nil, errors.New("not sent: this assistant can't share bookings")
	}
	issued, err := g.Grant(ctx, d.Grant.Carrier, d.Grant.BookingRef, shipper.GrantRequest{Grantee: d.Grant.Grantee, Scope: d.Grant.Scope,
		Days: d.Grant.Days, Note: d.Grant.Note})
	if err != nil {
		return nil, a.notSent(d, err)
	}
	return &Sent{Draft: d, Granted: issued}, nil
}
