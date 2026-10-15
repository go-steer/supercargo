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
	"strings"
	"time"

	"github.com/go-steer/supercargo/internal/business"
	"github.com/go-steer/supercargo/internal/directory"
	"github.com/go-steer/supercargo/internal/llm"
)

// The fake model's door-to-door order, as the demo phrases it ("Ask
// Meridian Forwarding to ship one 45-footer of the launch sneakers from
// our Tilburg warehouse to the Cajamar DC, door to door"): it reads our
// private context, then our partners and places, then drafts the order
// with slack before the private deadline, as a model would.

// orderPattern and shipPattern: a door-to-door order to place.
var (
	orderPattern = regexp.MustCompile(`(?i)\bdoor[- ]to[- ]door\b`)
	shipPattern  = regexp.MustCompile(`(?i)\b(?:ship|order|send)\b`)
	// whyPattern: a question about an order, not one.
	whyPattern = regexp.MustCompile(`(?i)\b(?:why|late|delay(?:s|ed)?|where|status)\b`)
)

// SlackDays is how far before a private deadline the fake model asks the
// forwarder to deliver.
const SlackDays = 2

func fakeOrderStep(msgs []llm.Message, user string, done []step, now time.Time) llm.GenerateResponse {
	id := fmt.Sprintf("call-%d", len(msgs))
	if len(done) == 0 {
		return call(id, ToolBusiness, struct{}{})
	}
	last := done[len(done)-1]
	switch last.call.Name {
	case ToolBusiness:
		return call(id, ToolPartners, struct{}{})
	case ToolPartners:
		in, problem := orderFrom(msgs, user, done, now)
		if problem != "" {
			return say("%s", problem)
		}
		return call(id, ToolDraftOrder, in)
	case ToolDraftOrder:
		return orderDraftReply(last.result.Content)
	}
	return say("I couldn't prepare the order.")
}

// orderFrom reads the order out of the user's words, our places and our
// private context.
func orderFrom(msgs []llm.Message, user string, done []step, now time.Time) (map[string]any, string) {
	fwd, ok := partnerNamed(msgs, user, directory.RoleForwarder)
	if !ok {
		return nil, "Which forwarder should take it? Name one from our address book (or its domain)."
	}
	var places []struct{ ID, Name, UNLocode string }
	var biz struct{ Shipments []business.Shipment }
	for _, s := range done {
		switch s.call.Name {
		case ToolPartners:
			var l struct {
				OurPlaces []struct{ ID, Name, UNLocode string }
			}
			_ = json.Unmarshal([]byte(s.result.Content), &l)
			places = l.OurPlaces
		case ToolBusiness:
			_ = json.Unmarshal([]byte(s.result.Content), &biz)
		}
	}
	lower := strings.ToLower(user)
	// The places the user named, in the order named: from, then to.
	var named []string
	type at struct {
		pos int
		id  string
	}
	var found []at
	for _, p := range places {
		for _, w := range strings.Fields(strings.ToLower(p.Name)) {
			if len(w) >= 4 {
				if i := strings.Index(lower, w); i >= 0 {
					found = append(found, at{i, p.ID})
					break
				}
			}
		}
	}
	slices.SortFunc(found, func(x, y at) int { return x.pos - y.pos })
	for _, f := range found {
		named = append(named, f.id)
	}
	if len(named) < 2 {
		return nil, "From which of our places to which? Name both (list_partners lists our places)."
	}
	in := map[string]any{"forwarder": fwd.Domain, "from": named[0], "to": named[1], "incoterms": "DAP",
		"containerType": "40HC", "containerCount": 1, "grossWeightKg": 12000, "cargoDescription": "General cargo", "hsCode": "6404",
		"readyFrom": now.AddDate(0, 0, 1).Format(time.DateOnly), "deliverBy": now.AddDate(0, 0, 45).Format(time.DateOnly)}
	if strings.Contains(lower, "45") {
		in["containerType"] = "45HC"
	}
	if strings.Contains(lower, "sneaker") || strings.Contains(lower, "shoe") || strings.Contains(lower, "footwear") {
		in["cargoDescription"] = "Footwear (sneakers)"
	}
	// The shipment our private context has for this (its notes share the
	// user's words): its reference, and a deliver-by with slack before its
	// deadline. The deadline itself is never sent.
	for _, s := range biz.Shipments {
		if s.Notes == "" || !sharesWord(lower, strings.ToLower(s.Notes)) {
			continue
		}
		in["shipperRef"] = s.ShipperRef
		if d, err := time.Parse(time.DateOnly, s.RequiredBy); err == nil {
			in["deliverBy"] = d.AddDate(0, 0, -SlackDays).Format(time.DateOnly)
		}
		break
	}
	return in, ""
}

// sharesWord reports whether a and b share a word of five letters or more.
func sharesWord(a, b string) bool {
	for _, w := range strings.FieldsFunc(b, func(r rune) bool { return r == ' ' || r == ',' || r == ':' || r == '.' }) {
		if len(w) >= 5 && strings.Contains(a, w) {
			return true
		}
	}
	return false
}

// orderReportReply tells the user the forwarder's answer to an order.
func orderReportReply(result string) llm.GenerateResponse {
	var r struct {
		Forwarder string
		Answer    *struct {
			OrderRef string `json:"forwarder_reference"`
			Status   string `json:"status"`
		}
	}
	if json.Unmarshal([]byte(result), &r) != nil || r.Answer == nil {
		return say("I couldn't read the forwarder's answer.")
	}
	if r.Answer.Status == "pending" {
		return say("Ordered. %s took it as %s and is planning the legs; it will confirm with the journey.", r.Forwarder, r.Answer.OrderRef)
	}
	return say("%s answered %s: %s.", r.Forwarder, r.Answer.OrderRef, r.Answer.Status)
}

func orderDraftReply(content string) llm.GenerateResponse {
	var d DraftResult
	if json.Unmarshal([]byte(content), &d) != nil || d.Order == nil {
		return say("I couldn't prepare the order.")
	}
	o := d.Order
	out := fmt.Sprintf("Draft %s is ready for %s: door to door from %s to %s under %s, deliver by %s.",
		d.DraftID, d.To, o.PlaceOfReceipt.UNLocationCode, o.PlaceOfDelivery.UNLocationCode, o.Incoterms.Rule, strings.TrimSuffix(o.PlaceOfDelivery.Latest, "T12:00:00Z"))
	if by, ok := d.NotSent["requiredBy"]; ok {
		out += fmt.Sprintf(" That leaves %d days before your own deadline (%s), which stays with us: the forwarder sees only the deliver-by date.", SlackDays, by)
	}
	return say("%s The forwarder plans the legs and answers with the journey. Nothing is sent until you confirm.", out)
}
