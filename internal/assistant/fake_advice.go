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

	"github.com/go-steer/supercargo/internal/business"
	"github.com/go-steer/supercargo/internal/llm"
	"github.com/go-steer/supercargo/internal/shipper"
	"github.com/go-steer/supercargo/internal/store"
)

// The fake model asked what to do about a delay (the banner's "What can
// we do?"): it reads our private context and the booking's records,
// drafts the question it would put to the operator (nothing private in
// it; the person sends it from its dialog), and says what the delay means
// for us and the options.

// shipperAdvice: asked what to do, not what happened.
var shipperAdvice = regexp.MustCompile(`(?i)\bwhat (?:can|should|do|shall) we do\b`)

// expediteQuestion is what the fake asks an operator about expediting.
func expediteQuestion(ref, eta string) string {
	q := "The delivery of " + ref + " has moved"
	if eta != "" {
		q += " to " + eta
	}
	return q + ". Can any part of the journey still ahead be expedited, and what is the earliest possible delivery?"
}

func adviceStep(id, ref string, done []step) llm.GenerateResponse {
	switch len(done) {
	case 0:
		return call(id, ToolBusiness, struct{}{})
	case 1:
		return call(id, ToolGetEvents, map[string]string{"bookingRef": ref})
	}
	var recs shipper.RecordsResponse
	_ = json.Unmarshal([]byte(done[1].result.Content), &recs)
	if recs.Summary == nil {
		return say("I couldn't read %s's records.", ref)
	}
	day := recs.Summary.ETA[:min(10, len(recs.Summary.ETA))]
	last := done[len(done)-1]
	switch {
	case len(done) == 2:
		return call(id, ToolDraftQuestion, map[string]string{"bookingRef": ref, "question": expediteQuestion(ref, day)})
	case last.call.Name == ToolDraftQuestion && last.result.IsError && len(done) == 3:
		// The new date is a private one too: ask without it.
		return call(id, ToolDraftQuestion, map[string]string{"bookingRef": ref, "question": expediteQuestion(ref, "")})
	}
	var d DraftResult
	if last.call.Name == ToolDraftQuestion && !last.result.IsError {
		_ = json.Unmarshal([]byte(last.result.Content), &d)
	}
	var biz struct {
		Shipments []business.Shipment
		Notes     []string
	}
	_ = json.Unmarshal([]byte(done[0].result.Content), &biz)
	return say("%s", advice(ref, recs.Summary, biz.Shipments, biz.Notes, d))
}

// advice is what the delay means for us and the options.
func advice(ref string, s *store.Summary, shipments []business.Shipment, notes []string, d DraftResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s is now due %s (from %s).", ref, strings.Replace(strings.TrimSuffix(s.ETA, "Z"), "T", " ", 1)+" UTC", s.Operator)
	// Our private context for it, by our own reference.
	var mine *business.Shipment
	for i, sh := range shipments {
		if s.ShipperRef != "" && sh.ShipperRef == s.ShipperRef {
			mine = &shipments[i]
		}
	}
	if mine != nil && mine.RequiredBy != "" {
		if s.ETA[:min(10, len(s.ETA))] > mine.RequiredBy {
			fmt.Fprintf(&b, " That's after our own deadline, %s", mine.RequiredBy)
		} else {
			fmt.Fprintf(&b, " That's still before our own deadline, %s", mine.RequiredBy)
		}
		if mine.Notes != "" {
			fmt.Fprintf(&b, " (%s)", mine.Notes)
		}
		b.WriteString(", which stays with us.")
	}
	b.WriteString("\n\nWhat we could do:")
	if q := d.Question; q != nil {
		fmt.Fprintf(&b, "\n1. Ask %s whether it can be expedited. Draft %s holds the question, word for word: \"%s\" It names none of our private dates or notes; read it and send it if you agree.", q.Operator, d.DraftID, q.Question)
	} else {
		fmt.Fprintf(&b, "\n1. Ask %s whether it can be expedited.", s.Operator)
	}
	if mine != nil && mine.ReceivingSlot != nil {
		sl := mine.ReceivingSlot
		fmt.Fprintf(&b, "\n2. Move our receiving slot (%s %s, %s) to the new arrival: that's ours to change, nothing is sent.", sl.Date, sl.Window, sl.Warehouse)
	}
	for _, n := range notes {
		fmt.Fprintf(&b, "\n- Our note: %s.", strings.TrimSuffix(n, "."))
	}
	return b.String()
}

// questionReport tells the user a sent question's answer.
func questionReport(result string) llm.GenerateResponse {
	var r struct {
		Operator, Answer string
		Cited, Unsigned  []string
	}
	if json.Unmarshal([]byte(result), &r) != nil || r.Answer == "" {
		return say("I couldn't read the answer.")
	}
	out := fmt.Sprintf("%s answered: %s (sources: %s)", r.Operator, r.Answer, strings.Join(r.Cited, ", "))
	if len(r.Unsigned) > 0 {
		out += fmt.Sprintf(". Of those, %s came from an EDI status nobody signed", strings.Join(r.Unsigned, ", "))
	}
	return say("%s.", out)
}
