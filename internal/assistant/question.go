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
	"fmt"
	"strings"

	"github.com/go-steer/supercargo/internal/llm"
	"github.com/go-steer/supercargo/pkg/intents"
)

// A question offered as an option (after "what can we do?") is drafted,
// not asked: the person reads the exact text in its dialog and sends it,
// so what goes out is what they saw. ask_operator still asks when the
// user asks a question outright.

// ToolDraftQuestion prepares a question for a person to send.
const ToolDraftQuestion = "draft_question"

// DraftQuestionTool is the company's own assistant's.
var DraftQuestionTool = llm.Tool{Name: ToolDraftQuestion,
	Description: "Prepare a question to the carrier or forwarder of one of our bookings, for the user to read and send themselves: use it when you offer asking as an option (\"we could ask whether it can be expedited\"). Nothing is sent: the dialog shows the exact text, and only the user sends it. It must name none of our private dates or notes.",
	Schema:      json.RawMessage(`{"type":"object","properties":{"bookingRef":{"type":"string","maxLength":64},"question":{"type":"string","maxLength":1000}},"required":["bookingRef","question"],"additionalProperties":false}`)}

// QuestionDraft is a question drafted for a person to send: exactly what
// the operator will read.
type QuestionDraft struct {
	Operator   string `json:"operator"`
	BookingRef string `json:"bookingRef"`
	Question   string `json:"question"`
}

func (a *Assistant) draftQuestion(ctx context.Context, raw json.RawMessage) (any, error) {
	var in struct{ BookingRef, Question string }
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, refuse("%v", err)
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
		return nil, refuse("the question mentions %s, which is private to us; write it without it", p)
	}
	dr, err := a.Drafts.add(Draft{Question: &QuestionDraft{Operator: op, BookingRef: in.BookingRef, Question: q.Question}, Created: a.now()})
	if err != nil {
		return nil, refuse("%v", err)
	}
	return dr.Result(), nil
}

// sendQuestion asks a drafted question: the dialog's send.
func (a *Assistant) sendQuestion(ctx context.Context, d Draft) (*Sent, error) {
	q := d.Question
	// Checked again: the private context may have changed since.
	if p := a.privateIn(q.Question); p != "" {
		return nil, fmt.Errorf("not sent: the question mentions %s, which is private to us", p)
	}
	r, err := a.Node.Ask(ctx, q.Operator, q.BookingRef, q.Question)
	if err != nil {
		return nil, a.notSent(d, err)
	}
	if r == nil || r.Answer == nil {
		return nil, fmt.Errorf("%w (the node answered without the operator's answer)", ErrOutcomeUnknown)
	}
	return &Sent{Draft: d, Asked: r}, nil
}

// askedResult is a sent question's outcome for the model: the operator's
// answer (its words: data), what it cites, and which we hold.
func askedResult(s *Sent) map[string]any {
	r := s.Asked
	return map[string]any{"draftId": s.Draft.ID, "operator": r.Operator, "question": s.Draft.Question.Question, "answer": r.Answer.Answer,
		"cited": r.Answer.CitedEventIDs, "held": r.Held, "unsigned": r.Unsigned}
}
