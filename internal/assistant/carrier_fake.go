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
	"strconv"
	"strings"

	"github.com/go-steer/supercargo/internal/llm"
	"github.com/go-steer/supercargo/internal/llm/fake"
	"github.com/go-steer/supercargo/internal/operator"
	"github.com/go-steer/supercargo/internal/store"
)

// FakeCarrierModel is the fake model for the carrier's assistant: the
// demo's carrier scenes (which bookings are on a voyage, one booking's
// story, delay a booking or a voyage) through the same tools and checks.
func FakeCarrierModel() *fake.Model {
	return &fake.Model{Respond: func(req llm.GenerateRequest) (llm.GenerateResponse, error) {
		user, done := turnSoFar(req.Messages)
		return carrierStep(req.Messages, user, done), nil
	}}
}

var (
	voyagePattern = regexp.MustCompile(`\b[0-9]{1,3}[A-Z]\b`)
	// delayPattern: an amount with its unit as a word ("3 days", "12
	// hours"), so a voyage like 12D is never read as 12 days.
	delayPattern = regexp.MustCompile(`(?i)\b([0-9]{1,3}|one|two|three|four|five|six|seven)\s+(days?|hours?|hrs?)\b`)
	// delayWord: asked for, not asked about: a sentence starting "Delay…"
	// or "Disrupt…", or "prepare/draft a delay".
	delayWord = regexp.MustCompile(`(?i)(?:^|[.!?]\s+)(?:please\s+)?(?:delay|disrupt)\b|\b(?:prepare|draft)\b[^.?!]*\b(?:delay|disruption)\b`)
)

func carrierStep(msgs []llm.Message, user string, done []step) llm.GenerateResponse {
	id := "call-" + strconv.Itoa(len(msgs))
	var last *step
	if len(done) > 0 {
		last = &done[len(done)-1]
		if last.result.IsError {
			return say("That didn't work: %s", last.result.Content)
		}
	}
	ref := refPattern.FindString(user)
	switch {
	case strings.HasPrefix(user, notePrefix) && strings.Contains(user, `"proposal"`):
		return proposalReport(resultOf(user))
	case strings.HasPrefix(user, notePrefix) && strings.Contains(user, `"freightStatus"`):
		return rateReport(resultOf(user))
	case strings.HasPrefix(user, notePrefix):
		return disruptionReport(resultOf(user))
	case ref != "" && advicePattern.MatchString(user):
		return deskStep(id, ref, done)
	case ref != "" && freightPattern.MatchString(user):
		return freightStep(id, ref, user, done)
	case delayWord.MatchString(user) && (ref != "" || voyagePattern.MatchString(user)):
		if last != nil {
			return disruptionDraftReply(last.result.Content)
		}
		in := map[string]any{"delayHours": delayHours(user), "reason": reasonFrom(user)}
		if delayPattern.FindString(user) == "" {
			return say("How long is the delay? Tell me in days or hours.")
		}
		if ref != "" {
			in["bookingRef"] = ref
		} else {
			in["voyage"] = voyagePattern.FindString(user)
		}
		return call(id, ToolDraftDisruption, in)
	case ref != "":
		if last != nil {
			return bookingStoryReply(last.result.Content)
		}
		return call(id, ToolGetBooking, map[string]string{"bookingRef": ref})
	case strings.Contains(strings.ToLower(user), "booking") || voyagePattern.MatchString(user):
		if last != nil {
			return bookingsReply(last.result.Content)
		}
		in := map[string]string{}
		if v := voyagePattern.FindString(user); v != "" {
			in["voyage"] = v
		}
		return call(id, ToolListBookings, in)
	}
	return say("I can list our bookings (\"which bookings are on voyage 42W?\"), tell one's story (\"what's happening with OB-BKG-1042?\"), and prepare a delay for you to confirm (\"delay voyage 42W by 3 days: engine repair, unscheduled call at Halifax\").")
}

func delayHours(user string) int {
	m := delayPattern.FindStringSubmatch(user)
	if m == nil {
		return 24
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		n = map[string]int{"one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6, "seven": 7}[strings.ToLower(m[1])]
	}
	if strings.HasPrefix(strings.ToLower(m[2]), "d") {
		n *= 24
	}
	return n
}

// reasonFrom is the text after the first colon that follows the delay's
// amount ("by 3 days: engine repair"), else a plain one.
func reasonFrom(user string) string {
	if loc := delayPattern.FindStringIndex(user); loc != nil {
		if i := strings.Index(user[loc[1]:], ":"); i >= 0 {
			if r := strings.TrimSpace(user[loc[1]+i+1:]); r != "" {
				if !strings.HasSuffix(r, ".") {
					r += "."
				}
				return strings.ToUpper(r[:1]) + r[1:]
			}
		}
	}
	return "Operational delay."
}

func bookingsReply(content string) llm.GenerateResponse {
	var r struct{ Bookings []store.Summary }
	_ = json.Unmarshal([]byte(content), &r)
	if len(r.Bookings) == 0 {
		return say("No bookings match.")
	}
	var lines []string
	for _, s := range r.Bookings {
		lines = append(lines, fmt.Sprintf("- **%s** for `%s`: %s → %s, %s %s, %s, ETA %s", s.Ref, s.Shipper,
			s.Origin.UNLocode, s.Destination.UNLocode, s.Vessel, s.Voyage, s.Status, s.ETA))
	}
	return say("%d booking(s):\n\n%s", len(r.Bookings), strings.Join(lines, "\n"))
}

func bookingStoryReply(content string) llm.GenerateResponse {
	var r operator.BookingRecordsResponse
	_ = json.Unmarshal([]byte(content), &r)
	if r.Summary == nil {
		return say("I couldn't read that booking.")
	}
	s := r.Summary
	return say("**%s** for `%s`: %s, on %s %s, ETA %s. %d event(s) and %d alert(s) published; the latest: %s.",
		s.Ref, s.Shipper, s.Status, s.Vessel, s.Voyage, s.ETA, s.Events, s.Alerts, s.LastEvent)
}

func disruptionDraftReply(content string) llm.GenerateResponse {
	var d DraftResult
	if json.Unmarshal([]byte(content), &d) != nil || d.Disruption == nil {
		return say("I couldn't prepare that delay.")
	}
	scope := "booking " + d.Disruption.BookingRef
	if d.Disruption.Voyage != "" {
		scope = "voyage " + d.Disruption.Voyage
	}
	return say("I have prepared delay %s: %s, %s, for %s (%s). Reason: %q. Nothing is published yet: review it and confirm it in the dialog.",
		d.DraftID, scope, d.Disruption.Delay, strings.Join(d.Disruption.Bookings, ", "), plural(len(d.Disruption.Bookings), "booking"), d.Disruption.Reason)
}

func disruptionReport(result string) llm.GenerateResponse {
	var r struct{ Outcome operator.DisruptResponse }
	if json.Unmarshal([]byte(result), &r) != nil {
		return say("Done.")
	}
	alerts := r.Outcome.Alerts
	switch {
	case r.Outcome.Alert != nil:
		return say("Published: alert %s, ETA %s → %s.", r.Outcome.Alert.AlertID, r.Outcome.Alert.PreviousETA, r.Outcome.Alert.NewETA)
	case r.Outcome.Applied:
		return say("The delay is applied, but its alert didn't go out: don't publish it again.")
	case len(alerts) == 0:
		return say("Published, but nothing came back about the bookings: check them.")
	}
	var sent, failed []string
	for _, a := range alerts {
		switch {
		case a.Alert != nil:
			sent = append(sent, a.BookingRef+" ("+a.Alert.AlertID+")")
		case a.Applied:
			failed = append(failed, a.BookingRef+" was delayed but its alert didn't go out: don't publish again")
		default:
			failed = append(failed, a.BookingRef+" wasn't delayed")
		}
	}
	text := "Published: " + strings.Join(sent, ", ") + "."
	if len(failed) > 0 {
		text += " " + strings.Join(failed, "; ") + "."
	}
	return say("%s", text)
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}
