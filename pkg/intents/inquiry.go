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

package intents

import (
	"errors"
	"fmt"

	"github.com/go-steer/supercargo/pkg/internal/logsafe"
)

// Limits on inquiries and their answers.
const (
	MaxQuestionLen = 1000
	MaxAnswerLen   = 2000
	MaxCitations   = 20
)

// How an answer was produced.
const (
	GeneratedByLLM      = "llm"
	GeneratedByTemplate = "template"
)

// TnTInquiryBody is the body of ulp.tnt.inquiry: a free-text question
// about one booking (the header's bookingRef).
type TnTInquiryBody struct {
	Question string `json:"question"`
}

// Intent implements Body.
func (*TnTInquiryBody) Intent() string { return TnTInquiry }

// Validate implements Body. The question is one line: it reaches a model,
// and what a person reviews must be all there is.
func (b *TnTInquiryBody) Validate() error {
	return text("question", b.Question, MaxQuestionLen, false)
}

// TnTInquiryResponseBody is the body of ulp.tnt.inquiry.response: an
// answer grounded in the booking's records, citing the event and alert IDs
// it relies on, so the asker can check them against what it holds.
type TnTInquiryResponseBody struct {
	Answer        string    `json:"answer"`
	CitedEventIDs []string  `json:"citedEventIds"`
	GeneratedBy   string    `json:"generatedBy"`
	DataScope     DataScope `json:"dataScope"`
}

// DataScope says what the answer was drawn from.
type DataScope struct {
	BookingRef       string `json:"bookingRef"`
	EventsConsidered int    `json:"eventsConsidered"`
}

// Intent implements Body.
func (*TnTInquiryResponseBody) Intent() string { return TnTInquiryResponse }

// Validate implements Body.
func (b *TnTInquiryResponseBody) Validate() error {
	errs := []error{text("answer", b.Answer, MaxAnswerLen, true), ref("dataScope.bookingRef", b.DataScope.BookingRef)}
	if b.GeneratedBy != GeneratedByLLM && b.GeneratedBy != GeneratedByTemplate {
		errs = append(errs, fmt.Errorf("generatedBy: %s is not llm or template", logsafe.Quote(b.GeneratedBy)))
	}
	if b.DataScope.EventsConsidered < 0 {
		errs = append(errs, errors.New("dataScope.eventsConsidered is negative"))
	}
	errs = append(errs, citations("citedEventIds", b.CitedEventIDs))
	return errors.Join(errs...)
}

// citations checks a list of cited IDs: identifiers, unique, at most
// MaxCitations.
func citations(field string, ids []string) error {
	if len(ids) > MaxCitations {
		return fmt.Errorf("%s has %d entries, over %d", field, len(ids), MaxCitations)
	}
	var errs []error
	seen := map[string]bool{}
	for i, id := range ids {
		errs = append(errs, ref(fmt.Sprintf("%s[%d]", field, i), id))
		if seen[id] {
			errs = append(errs, fmt.Errorf("%s[%d] repeats %s", field, i, logsafe.Quote(id)))
		}
		seen[id] = true
	}
	return errors.Join(errs...)
}
