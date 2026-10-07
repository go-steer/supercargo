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
	"errors"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/llm/fake"
	"github.com/go-steer/supercargo/internal/shipper"
	"github.com/go-steer/supercargo/pkg/intents"
)

// booker is a node whose Book answers with resp or err.
type booker struct {
	node
	resp  *shipper.BookResponse
	err   error
	calls int
}

func (b *booker) Book(context.Context, shipper.BookRequest) (*shipper.BookResponse, error) {
	b.calls++
	return b.resp, b.err
}

func TestSend(t *testing.T) {
	confirmed := &shipper.BookResponse{Confirmation: &intents.BookingConfirmationBody{BookingRef: "OB-BKG-1", Status: intents.StatusConfirmed}}
	for name, c := range map[string]struct {
		resp    *shipper.BookResponse
		err     error
		wantErr error
		putBack bool
	}{
		"confirmed":          {resp: confirmed},
		"refused (4xx)":      {err: &NodeError{Status: 422, Message: "private"}, putBack: true},
		"carrier down (5xx)": {err: &NodeError{Status: 502, Message: "no answer"}, wantErr: ErrOutcomeUnknown},
		"no answer":          {err: errors.New("the node: EOF"), wantErr: ErrOutcomeUnknown},
		"no confirmation":    {resp: &shipper.BookResponse{}, wantErr: ErrOutcomeUnknown},
	} {
		b := &booker{resp: c.resp, err: c.err}
		a, _, _ := newAssistant(nil, &node{})
		a.Node = b
		d, _ := a.Drafts.add(Draft{Operator: "ocean-blue.example"})
		s, err := a.Send(context.Background(), d.ID)
		switch {
		case c.wantErr != nil && !errors.Is(err, c.wantErr):
			t.Errorf("%s: %v, want %v", name, err, c.wantErr)
		case c.wantErr == nil && c.err == nil && (err != nil || s.Confirmation.BookingRef != "OB-BKG-1"):
			t.Errorf("%s: %+v, %v", name, s, err)
		case c.putBack && (err == nil || !strings.HasPrefix(err.Error(), "not sent")):
			t.Errorf("%s: %v", name, err)
		}
		if _, err := a.Drafts.Get(d.ID); (err == nil) != c.putBack {
			t.Errorf("%s: draft back = %v, want %v", name, err == nil, c.putBack)
		}
		if b.calls != 1 {
			t.Errorf("%s: %d calls", name, b.calls)
		}
	}
	a, _, _ := newAssistant(nil, &node{})
	if _, err := a.Send(context.Background(), "BD-0404"); !errors.Is(err, ErrNoDraft) {
		t.Errorf("no draft: %v", err)
	}
}

// TestReport: the outcome goes to the model as a note, which never counts
// as the user naming a company.
func TestReport(t *testing.T) {
	a, _, _ := newAssistant(FakeModel(func() time.Time { return thursday }), &node{})
	var c Conversation
	for name, s := range map[string]*Sent{
		"before":   {Draft: Draft{ID: "BD-1", Operator: "mallory.example", RequiredBy: "2026-10-15"}, Confirmation: &intents.BookingConfirmationBody{BookingRef: "OB-1", Status: intents.StatusConfirmed, Vessel: &intents.Vessel{Name: "Aurora", Voyage: "42W"}, PlannedArrival: "2026-10-13T08:00:00Z"}},
		"same day": {Draft: Draft{ID: "BD-2", RequiredBy: "2026-10-13"}, Confirmation: &intents.BookingConfirmationBody{BookingRef: "OB-2", Status: intents.StatusConfirmed, PlannedArrival: "2026-10-13T08:00:00Z"}},
		"after":    {Draft: Draft{ID: "BD-3", RequiredBy: "2026-10-12"}, Confirmation: &intents.BookingConfirmationBody{BookingRef: "OB-3", Status: intents.StatusConfirmed, PlannedArrival: "2026-10-13T08:00:00Z"}},
		"rejected": {Draft: Draft{ID: "BD-4", Operator: "o.example"}, Confirmation: &intents.BookingConfirmationBody{Status: intents.StatusRejected}},
	} {
		r, err := a.Report(context.Background(), &c, s)
		want := map[string]string{"before": "which is 2 days before your deadline of 2026-10-15", "same day": "on the day of your deadline",
			"after": "after your deadline of 2026-10-12", "rejected": "didn't confirm the booking (status: rejected)"}[name]
		if err != nil || !strings.Contains(r.Text, want) {
			t.Errorf("%s: %q, %v", name, r.Text, err)
		}
	}
	if err := c.named("mallory.example"); err == nil {
		t.Error("a note counted as the user naming a company")
	}
	if r := reportReply("nonsense"); !strings.Contains(r.Text, "couldn't read") {
		t.Errorf("%q", r.Text)
	}
	if r := reportReply(`{"requiredBy":"2026-10-14","confirmation":{"status":"confirmed","plannedArrival":"2026-10-13T08:00:00Z"}}`); !strings.Contains(r.Text, "the day before") {
		t.Errorf("%q", r.Text)
	}
}

// TestReportFailureKept: when the model can't be told, the conversation
// still records that the booking was sent.
func TestReportFailureKept(t *testing.T) {
	a, _, _ := newAssistant(fake.Failing(fake.ErrUnavailable), &node{})
	var c Conversation
	s := &Sent{Draft: Draft{ID: "BD-1"}, Confirmation: &intents.BookingConfirmationBody{BookingRef: "OB-1", Status: intents.StatusConfirmed}}
	if _, err := a.Report(context.Background(), &c, s); err == nil {
		t.Fatal("no error")
	}
	if len(c.messages) != 2 || !strings.Contains(c.messages[0].Text, "BD-1") || c.messages[1].Text != "Noted." || len(c.turns) != 1 || c.userTexts[0] != "" {
		t.Errorf("history %+v", c.messages)
	}
	c.busy = true
	if err := c.Note("x"); !errors.Is(err, ErrBusy) {
		t.Error("a note while busy")
	}
}

func TestSendNodeDown(t *testing.T) {
	a, _, _ := newAssistant(nil, &node{})
	a.Node = &booker{err: &url.Error{Op: "Post", URL: "http://control/book", Err: &net.OpError{Op: "dial", Err: errors.New("no such file")}}}
	d, _ := a.Drafts.add(Draft{})
	if _, err := a.Send(context.Background(), d.ID); err == nil || !strings.Contains(err.Error(), "is the node running") {
		t.Errorf("%v", err)
	}
	if _, err := a.Drafts.Get(d.ID); err != nil {
		t.Error("the draft wasn't put back")
	}
}
