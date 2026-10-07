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
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/go-steer/supercargo/internal/llm"
	"github.com/go-steer/supercargo/internal/shipper"
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
)

// containerTypes are the container types a draft may ask for: the label
// people use, and its ISO 6346 size-type code.
var containerTypes = map[string]string{
	"20GP": "22G1", "40GP": "42G1", "40HC": "45G1", "45HC": "L5G1", "20RF": "22R1", "40RF": "45R1",
}

// Tools are the assistant's tools. There is no tool that sends a booking:
// sending happens only in the confirmation dialog, after a person says yes.
var Tools = []llm.Tool{
	{Name: ToolGetProfile, Description: "Fetch a company's ULP profile (services, intents, endpoints, signing keys) through our node, which verifies it. Use it when the user names a carrier's domain.",
		Schema: json.RawMessage(`{"type":"object","properties":{"domain":{"type":"string","maxLength":253,"description":"The company's domain, as the user wrote it, e.g. ocean-blue.example"}},"required":["domain"],"additionalProperties":false}`)},
	{Name: ToolDraftBooking, Description: "Prepare a booking request draft. Nothing is sent: the user reviews the draft and confirms it themselves. Returns the draft ID and what the carrier would receive.",
		Schema: json.RawMessage(`{"type":"object","properties":{` +
			`"operator":{"type":"string","maxLength":253,"description":"The carrier's domain, as the user named it"},` +
			`"shipperRef":{"type":"string","maxLength":64,"description":"The user's own reference (a PO number), if they gave one"},` +
			`"origin":{"type":"object","properties":{"unLocode":{"type":"string","maxLength":5},"name":{"type":"string","maxLength":128}},"required":["unLocode","name"],"additionalProperties":false},` +
			`"destination":{"type":"object","properties":{"unLocode":{"type":"string","maxLength":5},"name":{"type":"string","maxLength":128}},"required":["unLocode","name"],"additionalProperties":false},` +
			`"containerType":{"type":"string","enum":["20GP","40GP","40HC","45HC","20RF","40RF"]},` +
			`"containerCount":{"type":"integer","minimum":1,"maximum":999},` +
			`"cargoDescription":{"type":"string","maxLength":512},` +
			`"hsCode":{"type":"string","maxLength":10,"description":"The HS code's first 4 to 10 digits"},` +
			`"earliestDeparture":{"type":"string","maxLength":10,"description":"YYYY-MM-DD"},` +
			`"consigneeName":{"type":"string","maxLength":128},` +
			`"consigneeDomain":{"type":"string","maxLength":253,"description":"Only a domain the user gave"},` +
			`"requiredBy":{"type":"string","maxLength":10,"description":"YYYY-MM-DD: the user's own delivery deadline. Private: kept on our side, never sent"}` +
			`},"required":["operator","origin","destination","containerType","containerCount","cargoDescription","hsCode","earliestDeparture"],"additionalProperties":false}`)},
	{Name: ToolList, Description: "List our bookings, each at a glance: status, ETA, latest event, alerts.",
		Schema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)},
	{Name: ToolGetEvents, Description: "Get one booking's signed track-and-trace events and alerts, oldest first.",
		Schema: json.RawMessage(`{"type":"object","properties":{"bookingRef":{"type":"string","maxLength":64},"operator":{"type":"string","maxLength":253,"description":"Only needed if two carriers use the same reference"}},"required":["bookingRef"],"additionalProperties":false}`)},
	{Name: ToolStatus, Description: "Ask the carrier for one of our bookings' current DCSA status (structured, signed, no model involved): status, vessel, current location, ETA and all its events, including any we missed.",
		Schema: json.RawMessage(`{"type":"object","properties":{"bookingRef":{"type":"string","maxLength":64}},"required":["bookingRef"],"additionalProperties":false}`)},
	{Name: ToolAsk, Description: "Ask the carrier a question about one of our bookings. Our node sends it signed; the carrier's agent answers from that booking's records and cites them.",
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
		if err := c.named(in.Domain); err != nil {
			return nil, err
		}
		return a.Node.Profile(ctx, in.Domain)
	case ToolDraftBooking:
		return a.draft(c, call.Input)
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
	if st.Status == nil || len(st.Status.Events) <= maxRecords {
		return out
	}
	trimmed := *st.Status
	out.OmittedEvents = len(trimmed.Events) - maxRecords
	trimmed.Events = trimmed.Events[out.OmittedEvents:]
	kept := map[string]bool{}
	for _, e := range trimmed.Events {
		kept[e.EventID] = true
	}
	resp := *st
	resp.Status, resp.Missing = &trimmed, nil
	for _, id := range st.Missing {
		if kept[id] {
			resp.Missing = append(resp.Missing, id)
		}
	}
	out.StatusResponse = &resp
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
	Operator          string           `json:"operator"`
	ShipperRef        string           `json:"shipperRef"`
	Origin            intents.Location `json:"origin"`
	Destination       intents.Location `json:"destination"`
	ContainerType     string           `json:"containerType"`
	ContainerCount    int              `json:"containerCount"`
	CargoDescription  string           `json:"cargoDescription"`
	HSCode            string           `json:"hsCode"`
	EarliestDeparture string           `json:"earliestDeparture"`
	ConsigneeName     string           `json:"consigneeName"`
	ConsigneeDomain   string           `json:"consigneeDomain"`
	RequiredBy        string           `json:"requiredBy"`
}

// DraftResult is what draft_booking returns to the model, and what the
// confirmation dialog shows.
type DraftResult struct {
	DraftID string                     `json:"draftId"`
	Sent    bool                       `json:"sent"` // always false
	To      string                     `json:"to"`
	Request intents.BookingRequestBody `json:"request"`
	// NotSent is the private part of the draft, which stays here.
	NotSent map[string]string `json:"notSent,omitempty"`
	Next    string            `json:"next"`
}

func (a *Assistant) draft(c *Conversation, raw json.RawMessage) (any, error) {
	var in draftInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, refuse("%v", err)
	}
	if err := c.named(in.Operator); err != nil {
		return nil, err
	}
	if in.Operator == a.Self.Domain {
		return nil, refuse("%s is us; a booking goes to a carrier", in.Operator)
	}
	iso, ok := containerTypes[in.ContainerType]
	if !ok {
		return nil, refuse("no container type %s", in.ContainerType)
	}
	req := intents.BookingRequestBody{
		ShipperRef: in.ShipperRef, Origin: in.Origin, Destination: in.Destination,
		Equipment:         []intents.Equipment{{ISOType: iso, Label: in.ContainerType, Count: in.ContainerCount}},
		Cargo:             intents.Cargo{Description: strings.TrimSpace(in.CargoDescription), HSCode: in.HSCode},
		EarliestDeparture: in.EarliestDeparture,
	}
	switch {
	case in.ConsigneeDomain != "":
		if err := c.named(in.ConsigneeDomain); err != nil {
			return nil, err
		}
		req.Consignee = &intents.Consignee{Name: in.ConsigneeName, Domain: in.ConsigneeDomain}
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
	dr, err := a.Drafts.add(Draft{Operator: in.Operator, Request: req, RequiredBy: in.RequiredBy, Created: a.now()})
	if err != nil {
		return nil, refuse("%v", err)
	}
	out := DraftResult{DraftID: dr.ID, To: dr.Operator, Request: dr.Request,
		Next: "Nothing has been sent. Tell the user the draft is ready for them to review and confirm; only they can send it."}
	if dr.RequiredBy != "" {
		out.NotSent = map[string]string{"requiredBy": dr.RequiredBy}
	}
	return out, nil
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
