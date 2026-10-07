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
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-steer/supercargo/internal/llm"
	"github.com/go-steer/supercargo/internal/llm/fake"
	"github.com/go-steer/supercargo/internal/ports"
	"github.com/go-steer/supercargo/internal/shipper"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/intents"
)

// FakeModel is the fake model (llm.provider: fake) for the assistant: a
// small rule-based stand-in that reads the conversation as a model would
// and calls the same tools, for the demo's scenes (discovery, booking,
// tracking, asking the carrier). Rehearsals and CI exercise the real tool
// loop, validation and refusals with no API key. now dates the bookings.
func FakeModel(now func() time.Time) *fake.Model {
	return &fake.Model{Respond: func(req llm.GenerateRequest) (llm.GenerateResponse, error) {
		user, done := turnSoFar(req.Messages)
		return fakeStep(req.Messages, user, done, now()), nil
	}}
}

// step is a tool call made this turn, with its result.
type step struct {
	call   llm.ToolCall
	result llm.ToolResult
}

// turnSoFar returns the turn's user text and the tool calls since.
func turnSoFar(msgs []llm.Message) (string, []step) {
	var done []step
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role == llm.User && m.Text != "" {
			return m.Text, done
		}
		if m.Role == llm.Assistant {
			for _, c := range m.ToolCalls {
				s := step{call: c}
				if i+1 < len(msgs) {
					for _, r := range msgs[i+1].ToolResults {
						if r.CallID == c.ID {
							s.result = r
						}
					}
				}
				done = append([]step{s}, done...)
			}
		}
	}
	return "", done
}

var (
	domainPattern  = regexp.MustCompile(`\b[a-z0-9-]+(?:\.[a-z0-9-]+)*\.[a-z]{2,}\b`)
	refPattern     = regexp.MustCompile(`\b[A-Z]{2,}-[A-Z]{2,}-[0-9]+\b`)
	poPattern      = regexp.MustCompile(`\b[A-Z]{2,}-PO-[0-9]+\b`)
	consignee      = regexp.MustCompile(`(?i)consignee is ([^,.(]+?)\s*(?:in [A-Za-z ]+?)?\s*\(([a-z0-9.-]+)\)`)
	dayPattern     = regexp.MustCompile(`(?i)before (?:[a-z]+ )?the ([1-9]|[12][0-9]|3[01])(?:st|nd|rd|th)`)
	askPattern     = regexp.MustCompile(`(?i)\b(?:ask|why)\b`)
	isoDatePattern = regexp.MustCompile(`\b20[0-9]{2}-[01][0-9]-[0-3][0-9]\b`)
	countPattern   = regexp.MustCompile(`(?i)\b([0-9]{1,3}|one|two|three|four|five)\s*(?:x\s*)?(?:20|40|45)`)
)

var portNames = []struct{ name, code string }{
	{"rotterdam", "NLRTM"}, {"newark", "USEWR"}, {"hamburg", "DEHAM"}, {"antwerp", "BEANR"},
	{"shanghai", "CNSHA"}, {"singapore", "SGSIN"}, {"felixstowe", "GBFXT"}, {"los angeles", "USLAX"},
}

func call(id, name string, input any) llm.GenerateResponse {
	b, _ := json.Marshal(input)
	return llm.GenerateResponse{ToolCalls: []llm.ToolCall{{ID: id, Name: name, Input: b}}}
}

func say(format string, a ...any) llm.GenerateResponse {
	return llm.GenerateResponse{Text: fmt.Sprintf(format, a...)}
}

func fakeStep(msgs []llm.Message, user string, done []step, now time.Time) llm.GenerateResponse {
	lower := strings.ToLower(user)
	id := "call-" + strconv.Itoa(len(msgs))
	var last *step
	if len(done) > 0 {
		last = &done[len(done)-1]
		if last.result.IsError {
			return say("That didn't work: %s", last.result.Content)
		}
	}
	switch {
	case strings.Contains(lower, "book "):
		if last != nil {
			return draftReply(last.result.Content)
		}
		return call(id, ToolDraftBooking, draftFrom(msgs, user, now))
	case askPattern.MatchString(user):
		ref := refPattern.FindString(user)
		switch {
		case last != nil && last.call.Name == ToolAsk:
			return askReply(last.result.Content)
		case ref == "" && last == nil:
			return call(id, ToolList, struct{}{})
		case ref == "":
			if ref = firstRef(last.result.Content); ref == "" {
				return say("We have no bookings to ask about yet.")
			}
		}
		// A fixed question: the user's own words may carry private dates.
		return call(id, ToolAsk, map[string]string{"bookingRef": ref,
			"question": "What is causing the delay on " + ref + ", and how firm is the new ETA?"})
	case strings.Contains(lower, "where") || strings.Contains(lower, "status") || strings.Contains(lower, "shipment"):
		ref := refPattern.FindString(user)
		switch {
		case last != nil && last.call.Name == ToolGetEvents:
			return eventsReply(last.result.Content)
		case ref == "" && last == nil:
			return call(id, ToolList, struct{}{})
		case ref == "":
			if ref = firstRef(last.result.Content); ref == "" {
				return say("We have no bookings yet.")
			}
		}
		return call(id, ToolGetEvents, map[string]string{"bookingRef": ref})
	case domainPattern.MatchString(lower):
		if last != nil {
			return profileReply(last.result.Content)
		}
		return call(id, ToolGetProfile, map[string]string{"domain": domainPattern.FindString(lower)})
	}
	return say("I can look up a carrier (give me its domain), prepare a booking for you to confirm, and follow our shipments.")
}

// draftFrom reads a booking request out of the user's words, the way the
// demo phrases it.
func draftFrom(msgs []llm.Message, user string, now time.Time) map[string]any {
	lower := strings.ToLower(user)
	in := map[string]any{"containerCount": 1, "containerType": "40GP"}
	// The operator: the carrier looked up earlier, else a domain named
	// that isn't the consignee's.
	var cons []string
	if m := consignee.FindStringSubmatch(user); m != nil {
		cons = m
		in["consigneeName"], in["consigneeDomain"] = strings.TrimSpace(m[1]), m[2]
	}
	for _, m := range msgs {
		for _, c := range m.ToolCalls {
			if c.Name == ToolGetProfile {
				var p struct{ Domain string }
				_ = json.Unmarshal(c.Input, &p)
				in["operator"] = p.Domain
			}
		}
	}
	if in["operator"] == nil {
		for _, d := range domainPattern.FindAllString(lower, -1) {
			if cons == nil || d != cons[2] {
				in["operator"] = d
				break
			}
		}
	}
	type at struct {
		i          int
		name, code string
	}
	var found []at
	for _, p := range portNames {
		if i := strings.Index(lower, p.name); i >= 0 {
			found = append(found, at{i, p.name, p.code})
		}
	}
	slices.SortFunc(found, func(a, b at) int { return a.i - b.i })
	if len(found) >= 2 {
		in["portOfLoading"], in["portOfDischarge"] = found[0].code, found[1].code
	}
	in["grossWeightKg"] = 12000 // a plausible estimate for a box of shoes
	switch {
	case strings.Contains(lower, "high-cube") || strings.Contains(lower, "high cube") || strings.Contains(lower, "40hc"):
		in["containerType"] = "40HC"
	case strings.Contains(lower, "20ft") || strings.Contains(lower, "20gp"):
		in["containerType"] = "20GP"
	}
	if m := countPattern.FindStringSubmatch(lower); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			in["containerCount"] = n
		} else {
			in["containerCount"] = slices.Index([]string{"", "one", "two", "three", "four", "five"}, m[1])
		}
	}
	switch {
	case strings.Contains(lower, "sneaker"):
		in["cargoDescription"], in["hsCode"] = "Footwear (sneakers)", "6404"
	case strings.Contains(lower, "shoe") || strings.Contains(lower, "footwear"):
		in["cargoDescription"], in["hsCode"] = "Footwear", "6404"
	default:
		in["cargoDescription"], in["hsCode"] = "General cargo", "9999"
	}
	dates := isoDatePattern.FindAllString(user, -1)
	switch {
	case len(dates) > 0:
		in["earliestDeparture"] = dates[0]
	default: // "next week": next Monday
		d := now.UTC().AddDate(0, 0, 1)
		for d.Weekday() != time.Monday {
			d = d.AddDate(0, 0, 1)
		}
		in["earliestDeparture"] = d.Format(time.DateOnly)
	}
	if m := dayPattern.FindStringSubmatch(user); m != nil {
		day, _ := strconv.Atoi(m[1])
		d := time.Date(now.Year(), now.Month(), day, 0, 0, 0, 0, time.UTC)
		if day < now.Day() {
			d = d.AddDate(0, 1, 0)
		}
		in["requiredBy"] = d.Format(time.DateOnly)
	}
	if po := poPattern.FindString(user); po != "" {
		in["shipperRef"] = po
	}
	return in
}

func firstRef(content string) string {
	var r struct{ Shipments []store.Summary }
	if json.Unmarshal([]byte(content), &r) != nil || len(r.Shipments) == 0 {
		return ""
	}
	return r.Shipments[len(r.Shipments)-1].Ref
}

var intentWords = map[string]string{
	intents.BookingRequest: "container booking", intents.TnTEvent: "track & trace events",
	intents.TnTAlert: "proactive alerts", intents.TnTInquiry: "shipment inquiries",
}

func profileReply(content string) llm.GenerateResponse {
	var p shipper.Profile
	if json.Unmarshal([]byte(content), &p) != nil || p.Domain == "" {
		return say("I couldn't read that profile.")
	}
	var can []string
	for _, in := range p.Intents {
		if w, ok := intentWords[in]; ok && !slices.Contains(can, w) {
			can = append(can, w)
		}
	}
	return say("%s (%s) supports %s. They publish signing key %s, so their messages to us can be verified.",
		p.Name, p.Domain, strings.Join(can, ", "), strings.Join(p.Keys, " and "))
}

func draftReply(content string) llm.GenerateResponse {
	var d DraftResult
	if json.Unmarshal([]byte(content), &d) != nil || d.DraftID == "" || len(d.Request.Equipment) == 0 {
		return say("I couldn't prepare the draft.")
	}
	r := d.Request
	e := r.Equipment[0]
	out := fmt.Sprintf("Draft %s is ready: %d × %s of %s, %s (%s) → %s (%s), departing on or after %s, with %s.",
		d.DraftID, e.Count, ports.SizeLabel(e.ISOType), r.Cargo.Description, ports.Name(r.PortOfLoading), r.PortOfLoading,
		ports.Name(r.PortOfDischarge), r.PortOfDischarge, r.EarliestDeparture, d.To)
	if rb := d.NotSent["requiredBy"]; rb != "" {
		out += fmt.Sprintf(" Your deadline of %s stays with us.", rb)
	}
	return say("%s Nothing has been sent: review it and confirm to send.", out)
}

func eventsReply(content string) llm.GenerateResponse {
	var r shipper.RecordsResponse
	if json.Unmarshal([]byte(content), &r) != nil || r.Summary == nil {
		return say("I couldn't read that booking's events.")
	}
	s := r.Summary
	out := fmt.Sprintf("%s with %s: %s, ETA %s.", s.Ref, s.Operator, s.Status, s.ETA)
	for i := len(r.Records) - 1; i >= 0; i-- {
		if r.Records[i].Intent == intents.TnTAlert {
			var a intents.TnTAlertBody
			if json.Unmarshal(r.Records[i].Body, &a) == nil {
				out += fmt.Sprintf(" Latest alert (%s): %s", a.AlertID, a.Summary)
			}
			break
		}
	}
	return say("%s", out)
}

func askReply(content string) llm.GenerateResponse {
	var r shipper.AskResponse
	if json.Unmarshal([]byte(content), &r) != nil || r.Answer == nil {
		return say("The carrier didn't answer.")
	}
	return say("%s answered: %s (sources: %s)", r.Operator, r.Answer.Answer, strings.Join(r.Answer.CitedEventIDs, ", "))
}
