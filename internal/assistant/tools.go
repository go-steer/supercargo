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
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/go-steer/supercargo/internal/llm"
	"github.com/go-steer/supercargo/internal/ports"
	"github.com/go-steer/supercargo/internal/shipper"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
)

// Tool names.
const (
	ToolGetProfile   = "get_operator_profile"
	ToolDraftBooking = "draft_booking"
	ToolList         = "list_shipments"
	ToolGetEvents    = "get_events"
	ToolAsk          = "ask_operator"
	ToolStatus       = "get_status"
	ToolPartners     = "list_partners"
	ToolFind         = "find_partners"
)

// Tools are the assistant's tools. There is no tool that sends a booking:
// sending happens only in the confirmation dialog, after a person says yes.
var Tools = []llm.Tool{
	{Name: ToolGetProfile, Description: "Fetch a company's ULP profile (intents, capabilities, supported dialects, endpoints, signing keys) through our node, which verifies it. Use it when the user names a carrier.",
		Schema: json.RawMessage(`{"type":"object","properties":{"domain":{"type":"string","maxLength":253,"description":"The company's domain: as the user wrote it, or the domain of a partner the user named, from list_partners or find_partners"}},"required":["domain"],"additionalProperties":false}`)},
	{Name: ToolPartners, Description: "List the partners we know: our address book (carriers, forwarders, shippers, consignees, with their domains), our own places (doors and ports, by ID, for an order), the carriers we've booked with, and partners found in this conversation. Use it to find the domain of a company the user names by name.",
		Schema: json.RawMessage(`{"type":"object","properties":{"role":{"type":"string","enum":["carrier","shipper","consignee","forwarder"]}},"additionalProperties":false}`)},
	{Name: ToolFind, Description: "Search the partner directory for companies we don't know yet, e.g. carriers serving a lane. Each candidate's profile is fetched from its domain and verified; only verified ones can be used, and only once the user names one.",
		Schema: json.RawMessage(`{"type":"object","properties":{` +
			`"role":{"type":"string","enum":["carrier","shipper","consignee","forwarder"],"description":"Default carrier"},` +
			`"mode":{"type":"string","enum":["OCEAN","ROAD","RAIL","AIR"]},` +
			`"from":{"type":"string","maxLength":5,"description":"UN/LOCODE of the port of loading, e.g. NLRTM"},` +
			`"to":{"type":"string","maxLength":5,"description":"UN/LOCODE of the port of discharge, e.g. USEWR"},` +
			`"text":{"type":"string","maxLength":100,"description":"Words in the name or description"}` +
			`},"additionalProperties":false}`)},
	{Name: ToolDraftBooking, Description: "Prepare a booking request draft. Nothing is sent: the user reviews the draft and confirms it themselves. Returns the draft ID and what the carrier would receive.",
		Schema: json.RawMessage(`{"type":"object","properties":{` +
			`"operator":{"type":"string","maxLength":253,"description":"The carrier's domain: as the user wrote it, or the domain of the carrier the user named, from list_partners or find_partners"},` +
			`"shipperRef":{"type":"string","maxLength":64,"description":"The user's own reference (a PO number), if they gave one"},` +
			`"portOfLoading":{"type":"string","maxLength":5,"description":"UN/LOCODE, e.g. NLRTM for Rotterdam"},` +
			`"portOfDischarge":{"type":"string","maxLength":5,"description":"UN/LOCODE, e.g. USEWR for Newark"},` +
			`"grossWeightKg":{"type":"number","minimum":1,"maximum":40000,"description":"Each container's gross weight in kg (estimate if the user didn't say)"},` +
			`"containerType":{"type":"string","enum":["20GP","40GP","40HC","45HC","20RF","40RF"]},` +
			`"containerCount":{"type":"integer","minimum":1,"maximum":999},` +
			`"cargoDescription":{"type":"string","maxLength":512},` +
			`"hsCode":{"type":"string","maxLength":10,"description":"The HS code's first 4 to 10 digits"},` +
			`"earliestDeparture":{"type":"string","maxLength":10,"description":"YYYY-MM-DD"},` +
			`"consigneeName":{"type":"string","maxLength":128},` +
			`"consigneeDomain":{"type":"string","maxLength":253,"description":"The consignee's domain: as the user wrote it, or from list_partners for a consignee the user named"},` +
			`"requiredBy":{"type":"string","maxLength":10,"description":"YYYY-MM-DD: the user's own delivery deadline. Private: kept on our side, never sent"}` +
			`},"required":["operator","portOfLoading","portOfDischarge","containerType","containerCount","grossWeightKg","cargoDescription","hsCode","earliestDeparture"],"additionalProperties":false}`)},
	{Name: ToolList, Description: "List our bookings, each at a glance: status, ETA, latest event, alerts.",
		Schema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)},
	{Name: ToolGetEvents, Description: "Get one booking's signed track-and-trace events and alerts, oldest first.",
		Schema: json.RawMessage(`{"type":"object","properties":{"bookingRef":{"type":"string","maxLength":64},"operator":{"type":"string","maxLength":253,"description":"Only needed if two carriers use the same reference"}},"required":["bookingRef"],"additionalProperties":false}`)},
	{Name: ToolStatus, Description: "Ask the carrier for one of our bookings' current DCSA status (structured, signed, no model involved): status, vessel, current location, ETA and all its events, including any we missed.",
		Schema: json.RawMessage(`{"type":"object","properties":{"bookingRef":{"type":"string","maxLength":64}},"required":["bookingRef"],"additionalProperties":false}`)},
	{Name: ToolAsk, Description: "Ask the carrier a question about one of our bookings. Our node sends it signed; the carrier's (or forwarder's) agent answers from that booking's records and cites them. The result says which cited records we hold signed copies of (held) and which of those came from a source nobody signed, a forwarder's re-issued EDI status (unsigned): tell the user which facts are unsigned.",
		Schema: json.RawMessage(`{"type":"object","properties":{"bookingRef":{"type":"string","maxLength":64},"question":{"type":"string","maxLength":1000}},"required":["bookingRef","question"],"additionalProperties":false}`)},
}

// maxRecords bounds the records get_events returns (the latest), and
// maxShipments the bookings list_shipments does.
const (
	maxRecords   = 30
	maxShipments = 50
)

// privateIn returns the private text question contains, if any: an open
// draft's required-by date or one of a.Private, in the forms people write
// dates ("2026-10-15", "Oct 15", "15 October", "the 15th"), compared
// without case.
func (a *Assistant) privateIn(question string) string {
	q := strings.ToLower(question)
	dates, texts := a.Drafts.requiredBy(), []string(nil)
	for _, p := range a.Private {
		if _, err := time.Parse(time.DateOnly, p); err == nil {
			dates = append(dates, p)
		} else if strings.TrimSpace(p) != "" {
			texts = append(texts, p)
		}
	}
	for _, p := range texts {
		if strings.Contains(q, strings.ToLower(strings.TrimSpace(p))) {
			return "a note from our business context"
		}
	}
	for _, d := range dates {
		t, _ := time.Parse(time.DateOnly, d)
		day := strconv.Itoa(t.Day())
		forms := []string{d, t.Format("Jan 2"), t.Format("January 2"), t.Format("2 Jan"), t.Format("2 January"),
			"the " + day + ordinal(t.Day()), day + ordinal(t.Day())}
		for _, f := range forms {
			f = strings.ToLower(f)
			for i := 0; ; {
				j := strings.Index(q[i:], f)
				if j < 0 {
					break
				}
				start, end := i+j, i+j+len(f)
				if (start == 0 || !isDigit(q[start-1])) && (end == len(q) || !isDigit(q[end])) {
					return "the date " + d
				}
				i = start + 1
			}
		}
	}
	return ""
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

func ordinal(day int) string {
	switch {
	case day >= 11 && day <= 13:
		return "th"
	case day%10 == 1:
		return "st"
	case day%10 == 2:
		return "nd"
	case day%10 == 3:
		return "rd"
	}
	return "th"
}

// errTool is a refusal the model sees as the tool's error result.
type errTool struct{ msg string }

func (e errTool) Error() string { return e.msg }

func refuse(format string, a ...any) error { return errTool{fmt.Sprintf(format, a...)} }

// run runs a validated tool call and returns its result for the model.
func (a *Assistant) run(ctx context.Context, c *Conversation, t *turnState, call llm.ToolCall) (any, error) {
	switch call.Name {
	case ToolGetProfile:
		var in struct{ Domain string }
		_ = json.Unmarshal(call.Input, &in)
		if err := a.named(c, in.Domain); err != nil {
			return nil, err
		}
		return a.Node.Profile(ctx, in.Domain)
	case ToolDraftBooking:
		return a.draft(ctx, c, call.Input)
	case ToolDraftOrder:
		return a.draftOrder(ctx, c, call.Input)
	case ToolDraftQuestion:
		return a.draftQuestion(ctx, call.Input)
	case ToolBusiness:
		return a.businessContext()
	case ToolPartners:
		return a.listPartners(ctx, c, call.Input)
	case ToolFind:
		return a.findPartners(ctx, c, call.Input)
	case ToolList:
		s, err := a.Node.Shipments(ctx)
		if len(s) > maxShipments {
			s = s[len(s)-maxShipments:] // the latest
		}
		return map[string]any{"shipments": s}, err
	case ToolGetEvents:
		var in struct{ BookingRef, Operator string }
		_ = json.Unmarshal(call.Input, &in)
		op, err := a.operatorOf(ctx, in.BookingRef, in.Operator)
		if err != nil {
			return nil, err
		}
		r, err := a.Node.Records(ctx, op, in.BookingRef)
		if err != nil {
			return nil, err
		}
		if len(r.Records) > maxRecords {
			r.Records = r.Records[len(r.Records)-maxRecords:]
		}
		r.Records = withoutProvenance(r.Records)
		return r, nil
	case ToolStatus:
		var in struct{ BookingRef string }
		_ = json.Unmarshal(call.Input, &in)
		if t.statused[in.BookingRef] {
			return nil, refuse("you already have %s's status from this turn; use it", in.BookingRef)
		}
		op, err := a.operatorOf(ctx, in.BookingRef, "")
		if err != nil {
			return nil, err
		}
		t.statused[in.BookingRef] = true
		st, err := a.Node.Status(ctx, op, in.BookingRef)
		if err != nil {
			return nil, err
		}
		return trimStatus(st), nil
	case ToolAsk:
		var in struct{ BookingRef, Question string }
		_ = json.Unmarshal(call.Input, &in)
		switch {
		case t.asked[in.BookingRef]:
			return nil, refuse("you already asked about %s in this turn; use that answer", in.BookingRef)
		case len(t.asked) >= MaxAsksPerTurn:
			return nil, refuse("at most %d questions to carriers per message", MaxAsksPerTurn)
		}
		op, err := a.operatorOf(ctx, in.BookingRef, "")
		if err != nil {
			return nil, err
		}
		q := intents.TnTInquiryBody{Question: strings.TrimSpace(in.Question)}
		if err := q.Validate(); err != nil {
			return nil, refuse("the question can't be sent: %v", err)
		}
		if p := a.privateIn(q.Question); p != "" {
			return nil, refuse("not sent: the question mentions %s, which is private to us; ask without it", p)
		}
		t.asked[in.BookingRef] = true
		return a.Node.Ask(ctx, op, in.BookingRef, q.Question)
	}
	return nil, refuse("no tool %s", call.Name) // ValidateCall ran first
}

// statusResult is get_status's result: the report with its latest
// events (as get_events keeps), and how many older ones were left out.
type statusResult struct {
	*shipper.StatusResponse
	OmittedEvents int `json:"omitted_events,omitempty"`
}

// trimStatus keeps a report's latest maxRecords events, and only the
// missing IDs among them, so a large booking still fits a tool result.
func trimStatus(st *shipper.StatusResponse) statusResult {
	out := statusResult{StatusResponse: st}
	if st.Status == nil {
		return out
	}
	trimmed := *st.Status
	if n := len(trimmed.Events) - maxRecords; n > 0 {
		out.OmittedEvents = n
	}
	// A copy without derived_from: unchecked partner text, never for a model.
	trimmed.Events = slices.Clone(trimmed.Events[out.OmittedEvents:])
	for i := range trimmed.Events {
		trimmed.Events[i].DerivedFrom = nil
	}
	resp := *st
	resp.Status = &trimmed
	out.StatusResponse = &resp
	if out.OmittedEvents == 0 {
		return out
	}
	kept := map[string]bool{}
	for _, e := range trimmed.Events {
		kept[e.EventID] = true
	}
	resp.Missing = nil
	for _, id := range st.Missing {
		if kept[id] {
			resp.Missing = append(resp.Missing, id)
		}
	}
	return out
}

// operatorOf finds the operator of our booking ref (one of ours: a tool
// never reaches a booking the node doesn't hold).
func (a *Assistant) operatorOf(ctx context.Context, ref, operator string) (string, error) {
	all, err := a.Node.Shipments(ctx)
	if err != nil {
		return "", err
	}
	var found []string
	for _, s := range all {
		if s.Ref == ref && (operator == "" || s.Operator == operator) {
			found = append(found, s.Operator)
		}
	}
	switch len(found) {
	case 0:
		return "", refuse("we hold no booking %s", ref)
	case 1:
		return found[0], nil
	}
	return "", refuse("%s is a booking with %s; say which carrier", ref, strings.Join(found, " and "))
}

// draftInput is draft_booking's input.
type draftInput struct {
	Operator          string  `json:"operator"`
	ShipperRef        string  `json:"shipperRef"`
	PortOfLoading     string  `json:"portOfLoading"`
	PortOfDischarge   string  `json:"portOfDischarge"`
	GrossWeightKg     float64 `json:"grossWeightKg"`
	ContainerType     string  `json:"containerType"`
	ContainerCount    llm.Int `json:"containerCount"`
	CargoDescription  string  `json:"cargoDescription"`
	HSCode            string  `json:"hsCode"`
	EarliestDeparture string  `json:"earliestDeparture"`
	ConsigneeName     string  `json:"consigneeName"`
	ConsigneeDomain   string  `json:"consigneeDomain"`
	RequiredBy        string  `json:"requiredBy"`
}

// DraftResult is what draft_booking returns to the model, and what the
// confirmation dialog shows.
type DraftResult struct {
	DraftID string                      `json:"draftId"`
	Sent    bool                        `json:"sent"` // always false
	To      string                      `json:"to,omitempty"`
	Request *intents.BookingRequestBody `json:"request,omitempty"` // a booking's
	// Order is a door-to-door order's, to the forwarder To.
	Order *intents.ForwardingRequestBody `json:"order,omitempty"`
	// Proposal is a forwarder's confirmation of a delay's proposal.
	Proposal *ProposalDraft `json:"proposal,omitempty"`
	// Question is a question to a booking's operator, To.
	Question *QuestionDraft `json:"question,omitempty"`
	// Disruption is a carrier's drafted delay, instead of a booking.
	Disruption *Disruption `json:"disruption,omitempty"`
	// NotSent is the private part of the draft, which stays here.
	NotSent map[string]string `json:"notSent,omitempty"`
	// NewPartner: the carrier isn't in our address book, and we've never
	// booked with it. The dialog says so.
	NewPartner bool `json:"newPartner,omitempty"`
	// NewConsignee: the consignee isn't in our address book. Its domain
	// goes to the carrier, so the dialog says so too.
	NewConsignee bool   `json:"newConsignee,omitempty"`
	Next         string `json:"next"`
}

func (a *Assistant) draft(ctx context.Context, c *Conversation, raw json.RawMessage) (any, error) {
	var in draftInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, refuse("%v", err)
	}
	if err := a.named(c, in.Operator); err != nil {
		return nil, err
	}
	if in.Operator == a.Self.Domain {
		return nil, refuse("%s is us; a booking goes to a carrier", in.Operator)
	}
	iso := ports.SizeType(in.ContainerType)
	if iso == "" {
		return nil, refuse("no container type %s", in.ContainerType)
	}
	// The model may hold our private context: none of it in the request's
	// free text, however written.
	for field, text := range map[string]string{"cargoDescription": in.CargoDescription, "consigneeName": in.ConsigneeName, "shipperRef": in.ShipperRef} {
		if p := a.privateIn(text); p != "" {
			return nil, refuse("%s mentions %s, which is private to us: the booking must not carry it; leave it out", field, p)
		}
	}
	req := intents.BookingRequestBody{
		ShipperRef: in.ShipperRef, ShipperName: a.shipperName(), PortOfLoading: in.PortOfLoading, PortOfDischarge: in.PortOfDischarge,
		Equipment:         []intents.Equipment{{ISOType: iso, Count: int(in.ContainerCount), GrossWeightKg: in.GrossWeightKg}},
		Cargo:             intents.Cargo{Description: fold(in.CargoDescription), HSCode: in.HSCode},
		EarliestDeparture: in.EarliestDeparture,
	}
	switch {
	case in.ConsigneeDomain != "":
		if err := a.named(c, in.ConsigneeDomain); err != nil {
			return nil, err
		}
		req.Consignee = &intents.Consignee{Name: fold(in.ConsigneeName), Domain: in.ConsigneeDomain}
	case in.ConsigneeName != "":
		return nil, refuse("a consignee needs the domain the user gives for it; ask the user, or leave the consignee out")
	}
	if a.Self.Endpoint != "" {
		req.Subscription = &intents.Subscription{Endpoint: a.Self.Endpoint, Intents: []string{intents.TnTEvent, intents.TnTAlert}}
	}
	if in.RequiredBy != "" {
		if _, err := time.Parse(time.DateOnly, in.RequiredBy); err != nil {
			return nil, refuse("requiredBy %q is not a YYYY-MM-DD date", in.RequiredBy)
		}
	}
	probe := req
	if probe.ShipperRef == "" {
		probe.ShipperRef = "BD-0000" // filled in with the draft ID
	}
	if err := probe.Validate(); err != nil {
		return nil, refuse("the draft isn't a valid booking request: %v", err)
	}
	newPartner := !a.inAddressBook(in.Operator)
	if newPartner {
		if s, err := a.Node.Shipments(ctx); err == nil {
			newPartner = !slices.ContainsFunc(s, func(x store.Summary) bool { return x.Operator == in.Operator })
		}
	}
	newConsignee := req.Consignee != nil && req.Consignee.Domain != "" && !a.inAddressBook(req.Consignee.Domain)
	dr, err := a.Drafts.add(Draft{Operator: in.Operator, Request: req, RequiredBy: in.RequiredBy, NewPartner: newPartner,
		NewConsignee: newConsignee, Created: a.now()})
	if err != nil {
		return nil, refuse("%v", err)
	}
	return dr.Result(), nil
}

// Result is the draft as draft_booking returns it and the dialog shows it.
func (d Draft) Result() DraftResult {
	if d.Disruption != nil {
		dis := *d.Disruption
		dis.Bookings = slices.Clone(dis.Bookings)
		return DraftResult{DraftID: d.ID, Disruption: &dis,
			Next: "Nothing has been published. Tell the user the delay is ready for them to review and confirm; only they can publish it."}
	}
	if q := d.Question; q != nil {
		qd := *q
		return DraftResult{DraftID: d.ID, To: q.Operator, Question: &qd,
			Next: "Nothing has been sent. Tell the user the question is ready for them to read and send; only they can send it."}
	}
	if p := d.Proposal; p != nil {
		pd := *p
		return DraftResult{DraftID: d.ID, To: p.Carrier, Proposal: &pd,
			Next: "Nothing has been sent. Tell the user the proposal's confirmation is ready for them to review; only they can send the amendment."}
	}
	if o := d.Order; o != nil {
		req := o.Request
		return DraftResult{DraftID: d.ID, To: o.Forwarder, Order: &req, NotSent: maps.Clone(o.NotSent), NewPartner: d.NewPartner,
			Next: "Nothing has been sent. Tell the user the order is ready for them to review and confirm; only they can send it. Say which private dates stay with us."}
	}
	req := d.Request
	out := DraftResult{DraftID: d.ID, To: d.Operator, Request: &req, NewPartner: d.NewPartner, NewConsignee: d.NewConsignee,
		Next: "Nothing has been sent. Tell the user the draft is ready for them to review and confirm; only they can send it."}
	if d.RequiredBy != "" {
		out.NotSent = map[string]string{"requiredBy": d.RequiredBy}
	}
	return out
}

// errNotNamed is the counterparty rule: the assistant reaches only
// companies the user named.
var errNotNamed = errors.New("the user hasn't named that domain in this conversation; ask them for it")

// named checks that the user wrote domain in this conversation, as a
// whole word: the user's text is split at spaces and punctuation other
// than '.', '-' and '_' (letters of any script are part of a word), a
// word's trailing dots are dropped, and one word must equal domain.
func (c *Conversation) named(domain string) error {
	if !envelope.ValidDomain(domain) {
		return refuse("%q is not a domain", domain)
	}
	for _, text := range c.userTexts {
		words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
			return r != '.' && r != '-' && r != '_' && (unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r))
		})
		for _, w := range words {
			if strings.TrimRight(w, ".") == domain {
				return nil
			}
		}
	}
	return errTool{errNotNamed.Error()}
}

// shipperName names us in a booking request: our name, else our domain.
func (a *Assistant) shipperName() string {
	if n := fold(a.Self.Name); n != "" {
		return n
	}
	return a.Self.Domain
}

// fold collapses each run of whitespace (no-break spaces too) to one
// space, as the confirmation dialog displays text: what the person sees
// there is then byte for byte what is sent.
func fold(s string) string { return strings.Join(strings.Fields(s), " ") }

// withoutProvenance drops derived_from from event and alert bodies: a
// re-issuer's account of its sources is partner text the shipper can't
// check against its log, so it never reaches a model.
func withoutProvenance(recs []store.RecordView) []store.RecordView {
	out := make([]store.RecordView, len(recs))
	for i, r := range recs {
		out[i] = r
		var body intents.Body
		switch r.Intent {
		case intents.TnTEvent:
			e := &intents.TnTEventBody{}
			if json.Unmarshal(r.Body, e) == nil && e.DerivedFrom != nil {
				e.DerivedFrom = nil
				body = e
			}
		case intents.TnTAlert:
			a := &intents.TnTAlertBody{}
			if json.Unmarshal(r.Body, a) == nil && a.DerivedFrom != nil {
				a.DerivedFrom = nil
				body = a
			}
		}
		if body != nil {
			if b, err := json.Marshal(body); err == nil {
				out[i].Body = b
			}
		}
	}
	return out
}
