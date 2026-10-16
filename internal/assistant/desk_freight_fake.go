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
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-steer/supercargo/internal/forwarder"
	"github.com/go-steer/supercargo/internal/llm"
)

// The fake model at the forwarder's desk, asked why a leg can't be booked
// ("Why can't we book leg 3 of MF-0001?"): it checks the leg's freight,
// drafts a rate at or above the floor if it's below, and explains, from
// the tool results alone.

var (
	// freightPattern: asked about a leg's freight or why it won't book.
	freightPattern = regexp.MustCompile(`(?i)\b(?:can'?t|cannot|won'?t)\b[^?]*\bbook\b|\bfreight\b|\bfloor\b|\bANTT\b|\brate\b`)
	legPattern     = regexp.MustCompile(`(?i)\bleg\s+([1-8])\b`)
)

// freightStep checks the leg, then drafts a rate if it's below the floor.
func freightStep(id, ref, user string, done []step) llm.GenerateResponse {
	leg := 3
	if m := legPattern.FindStringSubmatch(user); m != nil {
		leg, _ = strconv.Atoi(m[1])
	}
	if len(done) == 0 {
		return call(id, ToolCheckFreight, map[string]any{"orderRef": ref, "leg": leg})
	}
	var v freightView
	if json.Unmarshal([]byte(done[0].result.Content), &v) != nil || v.Freight == nil {
		return say("I couldn't read leg %d's freight.", leg)
	}
	f := v.Freight
	if f.Status != forwarder.FreightBelow {
		if f.Floor == nil {
			return say("Leg %d (%s) can't be checked against the ANTT floor right now: %s. Our node won't book it unchecked; try again in a minute.", leg, v.Name, f.Error)
		}
		return say("Leg %d (%s) is fine: our rate R$ %.2f is at or above the ANTT floor of R$ %.2f (%s, per %s). Tolls R$ %.2f go to the carrier as a Vale-Pedágio.",
			leg, v.Name, f.Rate, f.Floor.Freight, f.Floor.Source, f.Floor.Resolution, f.Floor.TollTotal)
	}
	if len(done) == 1 {
		rate := math.Ceil(f.Floor.Freight/10)*10 + 30 // a round figure above the floor
		return call(id, ToolDraftRate, map[string]any{"orderRef": v.OrderRef, "leg": leg, "rate": rate})
	}
	var d DraftResult
	_ = json.Unmarshal([]byte(done[len(done)-1].result.Content), &d)
	var b strings.Builder
	fmt.Fprintf(&b, "Leg %d (%s, %s) is priced below Brazil's legal minimum freight. Our rate is R$ %.2f; the ANTT floor for %.1f km, %d axles, %s (table %s) is R$ %.2f, so we're R$ %.2f short (%s, per %s).",
		leg, v.Name, strings.Join(f.Route, " → "), f.Rate, f.Floor.DistanceKm, f.Axles, f.LoadType, f.Table, f.Floor.Freight, f.Shortfall, f.Floor.Source, f.Floor.Resolution)
	b.WriteString(" Contracting below it means owing the carrier twice the difference, and an ANTT fine; our node won't book it.")
	fmt.Fprintf(&b, " The %d toll plazas (R$ %.2f) aren't part of the freight: they're paid to the carrier ahead, as a Vale-Pedágio.", len(f.Floor.Tolls), f.Floor.TollTotal)
	if d.Rate != nil {
		fmt.Fprintf(&b, "\n\nDraft %s sets the leg's rate to R$ %.2f, above the floor: review and confirm it, then book the leg in the Orders tab.", d.DraftID, d.Rate.Rate)
	}
	return say("%s", b.String())
}

// rateReport tells the user what came of a confirmed rate.
func rateReport(result string) llm.GenerateResponse {
	var r struct {
		Order         string
		Leg           int
		Rate          float64
		FreightStatus string
	}
	if json.Unmarshal([]byte(result), &r) != nil || r.Order == "" {
		return say("I couldn't read the outcome.")
	}
	if r.FreightStatus == forwarder.FreightOK {
		return say("Done: leg %d of %s is now at R$ %.2f, above the floor. Book it in the Orders tab when you're ready.", r.Leg, r.Order, r.Rate)
	}
	return say("The rate for leg %d of %s is set to R$ %.2f, but the leg's freight is %s: check it again before booking.", r.Leg, r.Order, r.Rate, r.FreightStatus)
}
