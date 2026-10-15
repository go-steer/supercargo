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
	"slices"
	"strings"
	"testing"

	"github.com/go-steer/supercargo/internal/llm"
	"github.com/go-steer/supercargo/internal/shipper"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/intents"
)

// delayedNode holds one delayed order, by our reference, and answers
// questions about it.
type delayedNode struct {
	orderNode
	eta string
}

func (n *delayedNode) Shipments(context.Context) ([]store.Summary, error) {
	return []store.Summary{{Ref: "MF-0007", Operator: "meridian-forwarding.example", ShipperRef: "CY-PO-55120", ETA: n.eta}}, nil
}

func (n *delayedNode) Records(_ context.Context, operator, ref string) (*shipper.RecordsResponse, error) {
	n.log("records " + operator + " " + ref)
	return &shipper.RecordsResponse{Summary: &store.Summary{Ref: ref, Operator: operator, ShipperRef: "CY-PO-55120", ETA: n.eta}}, nil
}

func (n *delayedNode) Ask(_ context.Context, operator, ref, question string) (*shipper.AskResponse, error) {
	n.log("ask " + operator + " " + ref + " " + question)
	return &shipper.AskResponse{Operator: operator, Answer: &intents.TnTInquiryResponseBody{Answer: "Not before Nov 8.", CitedEventIDs: []string{"MF-AL-1"}},
		Held: []string{"MF-AL-1"}}, nil
}

const wantQuestion = "The delivery of MF-0007 has moved to 2026-11-08. Can any part of the journey still ahead be expedited, and what is the earliest possible delivery?"

// TestWhatCanWeDo: asked what to do about a delay, the assistant reads our
// private context and the order's records, drafts the question it would
// put to the forwarder (nothing private in it), and says the options;
// nothing is asked until the person sends the draft, which sends exactly
// what it shows.
func TestWhatCanWeDo(t *testing.T) {
	n := &delayedNode{orderNode: orderNode{node: &node{}}, eta: "2026-11-08T02:00:00Z"}
	a := orderingAssistant(t, n)
	var c Conversation
	r, err := a.Turn(context.Background(), &c, "What can we do about the delay on MF-0007?")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"after our own deadline, 2026-11-07", "Move our receiving slot (2026-11-06 08:00-12:00", "Draft QD-0001",
		`"` + wantQuestion + `"`} {
		if !strings.Contains(r.Text, want) {
			t.Errorf("the reply lacks %q:\n%s", want, r.Text)
		}
	}
	if n.called("ask") || len(r.Drafts) != 1 || r.Drafts[0].Question == nil || r.Drafts[0].Question.Question != wantQuestion {
		t.Fatalf("drafts %+v, asked %v", r.Drafts, n.called("ask"))
	}
	sent, err := a.Send(context.Background(), r.Drafts[0].DraftID)
	if err != nil || sent.Asked == nil {
		t.Fatalf("sent %+v %v", sent, err)
	}
	if !n.called("ask meridian-forwarding.example MF-0007 " + wantQuestion) {
		t.Errorf("asked %v, want exactly the drafted question", n.calls)
	}
	rep, err := a.Report(context.Background(), &c, sent)
	if err != nil || !strings.Contains(rep.Text, "Not before Nov 8") {
		t.Errorf("report %q %v", rep.Text, err)
	}
}

// TestWhatCanWeDoPrivateETA: a new date that is itself one of our private
// dates stays out of the question.
func TestWhatCanWeDoPrivateETA(t *testing.T) {
	n := &delayedNode{orderNode: orderNode{node: &node{}}, eta: "2026-11-07T02:00:00Z"}
	a := orderingAssistant(t, n)
	r, err := a.Turn(context.Background(), &Conversation{}, "What can we do about the delay on MF-0007?")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Drafts) != 1 || strings.Contains(r.Drafts[0].Question.Question, "2026-11-07") {
		t.Errorf("drafts %+v", r.Drafts)
	}
}

// TestDraftQuestionRefused: a question naming a private date or note, one
// about a booking that isn't ours, or no question at all: no draft. A
// draft that turned private since is refused at the send.
func TestDraftQuestionRefused(t *testing.T) {
	n := &delayedNode{orderNode: orderNode{node: &node{}}, eta: "2026-11-08T02:00:00Z"}
	a := orderingAssistant(t, n)
	for name, raw := range map[string]string{
		"the deadline":  `{"bookingRef":"MF-0007","question":"Can it arrive before Nov 7?"}`,
		"a note":        `{"bookingRef":"MF-0007","question":"Brazil launch: the São Paulo stores open with this stock. Can it be faster?"}`,
		"not ours":      `{"bookingRef":"OB-BKG-9999","question":"Can it be faster?"}`,
		"no question":   `{"bookingRef":"MF-0007","question":"   "}`,
		"not even JSON": `[`,
	} {
		if _, err := a.draftQuestion(context.Background(), json.RawMessage(raw)); err == nil {
			t.Errorf("%s: drafted", name)
		}
	}
	if len(a.Drafts.Open()) != 0 {
		t.Fatal("a draft was made")
	}
	r, err := a.draftQuestion(context.Background(), json.RawMessage(`{"bookingRef":"MF-0007","question":"Can it arrive on the 12th?"}`))
	if err != nil {
		t.Fatal(err)
	}
	a.Private = append(a.Private, "2026-11-12")
	if _, err := a.Send(context.Background(), r.(DraftResult).DraftID); err == nil || n.called("ask") {
		t.Errorf("a question turned private was sent: %v", err)
	}
	// No answer: may or may not have been asked.
	a.Private = a.Private[:len(a.Private)-1]
	r, _ = a.draftQuestion(context.Background(), json.RawMessage(`{"bookingRef":"MF-0007","question":"Can it be faster?"}`))
	a.Node = &silentAsk{n}
	if _, err := a.Send(context.Background(), r.(DraftResult).DraftID); !errors.Is(err, ErrOutcomeUnknown) {
		t.Errorf("no answer: %v", err)
	}
}

type silentAsk struct{ *delayedNode }

func (silentAsk) Ask(context.Context, string, string, string) (*shipper.AskResponse, error) {
	return &shipper.AskResponse{}, nil
}

// TestDraftQuestionTool: the company's own assistant drafts questions; an
// outside agent's session (no model of ours, MCP) can't.
func TestDraftQuestionTool(t *testing.T) {
	has := func(a *Assistant) bool {
		return slices.ContainsFunc(a.tools(), func(x llm.Tool) bool { return x.Name == ToolDraftQuestion })
	}
	own := orderingAssistant(t, &node{})
	outside := &Assistant{Node: &node{}, Drafts: &Drafts{}}
	if !has(own) || has(outside) {
		t.Errorf("own %v, outside %v", has(own), has(outside))
	}
	out := outside.RunTool(context.Background(), NewExternal(), ToolDraftQuestion, json.RawMessage(`{"bookingRef":"MF-0007","question":"Faster?"}`))
	if !out.IsError || len(outside.Drafts.Open()) != 0 {
		t.Errorf("over MCP: %+v", out)
	}
	if got := questionReport(`not json`).Text; !strings.Contains(got, "couldn't read") {
		t.Errorf("report %q", got)
	}
}
