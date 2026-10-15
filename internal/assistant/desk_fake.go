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
	"strings"

	"github.com/go-steer/supercargo/internal/llm"
	"github.com/go-steer/supercargo/internal/store"
)

// The fake model at the forwarder's desk, as the demo asks it ("What
// should we do about MF-0007?"): it reads the order, drafts confirming
// its open proposal if there is one, and says what happened and the
// options, from the tool results alone.

// advicePattern: asked what to do, not what happened.
var advicePattern = regexp.MustCompile(`(?i)\bwhat (?:should|do|can|shall) we do\b|\boptions\b|\bwhat now\b`)

// deskOrder is what the fake reads of get_order's result.
type deskOrder struct {
	OrderRef string `json:"orderRef"`
	Customer string `json:"customer"`
	Request  struct {
		PlaceOfDelivery struct {
			Latest string `json:"latest"`
		} `json:"place_of_delivery"`
	} `json:"request"`
	Legs    []deskLeg `json:"legs"`
	Journey struct {
		Legs   []store.Leg      `json:"legs"`
		Alerts []store.AlertRef `json:"alerts"`
	} `json:"journey"`
	Proposals []deskProposal `json:"proposals"`
}

func deskStep(id, ref string, done []step) llm.GenerateResponse {
	if len(done) == 0 {
		return call(id, ToolGetOrder, map[string]string{"orderRef": ref})
	}
	var o deskOrder
	if json.Unmarshal([]byte(done[0].result.Content), &o) != nil {
		return say("I couldn't read %s.", ref)
	}
	last := done[len(done)-1]
	if last.call.Name == ToolGetOrder {
		for _, p := range o.Proposals {
			if p.Status == "proposed" {
				return call(id, ToolDraftProposal, map[string]string{"orderRef": o.OrderRef, "proposalId": p.ID})
			}
		}
		return say("%s", deskAdvice(o, ""))
	}
	var d DraftResult
	_ = json.Unmarshal([]byte(last.result.Content), &d)
	return say("%s", deskAdvice(o, d.DraftID))
}

// deskAdvice says what happened to an order and the options.
func deskAdvice(o deskOrder, draft string) string {
	var b strings.Builder
	late := -1
	for i, l := range o.Journey.Legs {
		if l.Status == store.LegDelayed && late < 0 {
			late = i
		}
	}
	if late < 0 {
		fmt.Fprintf(&b, "%s is on plan: no leg is delayed.", o.OrderRef)
	} else {
		l := o.Journey.Legs[late]
		fmt.Fprintf(&b, "Leg %d (%s, %s → %s) is delayed: now %s, planned %s.", late+1, l.Carrier, l.FromPort.UNLocode, l.ToPort.UNLocode, l.Arrival.At, l.PlannedArrival)
	}
	if n := len(o.Journey.Alerts); n > 0 {
		a := o.Journey.Alerts[n-1]
		fmt.Fprintf(&b, " %s already has our alert %s: delivery now %s.", o.Customer, a.ID, a.NewETA)
	}
	if by := o.Request.PlaceOfDelivery.Latest; by != "" {
		last := o.Journey.Legs[len(o.Journey.Legs)-1].Arrival.At
		if last != "" && last <= by {
			fmt.Fprintf(&b, " That's still before the order's deliver-by (%s).", by)
		} else if last != "" {
			fmt.Fprintf(&b, " That's after the order's deliver-by (%s): tell the customer.", by)
		}
	}
	for _, l := range o.Legs {
		if l.Unsigned {
			fmt.Fprintf(&b, " %s (leg over EDI) reports unsigned statuses: its word, not its signature.", l.Name)
		}
	}
	for _, p := range o.Proposals {
		if p.Status != "proposed" {
			continue
		}
		fmt.Fprintf(&b, "\n\nOptions:\n1. Confirm proposal %s: %s", p.ID, p.Why)
		if draft != "" {
			fmt.Fprintf(&b, " Draft %s is ready for you to review and send.", draft)
		}
		b.WriteString("\n2. Leave leg " + fmt.Sprint(p.Leg) + " as booked: the truck would arrive before the cargo is there. Changing carrier means a second booking (ULP can't cancel a leg yet).")
		b.WriteString("\n\nI'd confirm 1.")
	}
	return b.String()
}

// proposalReport tells the user what came of a confirmed proposal.
func proposalReport(result string) llm.GenerateResponse {
	var r struct{ Proposal, Order, Status string }
	if json.Unmarshal([]byte(result), &r) != nil || r.Proposal == "" {
		return say("I couldn't read the outcome.")
	}
	switch r.Status {
	case "confirmed":
		return say("Done: the carrier took the new plan for %s (proposal %s).", r.Order, r.Proposal)
	case "refused":
		return say("The carrier kept its plan; proposal %s is closed. Check with them.", r.Proposal)
	}
	return say("Proposal %s is %s.", r.Proposal, r.Status)
}
