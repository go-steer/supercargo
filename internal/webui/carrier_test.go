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

package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/assistant"
	"github.com/go-steer/supercargo/internal/operator"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/intents"
)

// carrierNode is the carrier's node as its assistant sees it.
type carrierNode struct {
	mu        sync.Mutex
	disrupted []operator.DisruptRequest
}

func (n *carrierNode) Bookings(context.Context) ([]store.Summary, error) {
	return []store.Summary{{Ref: "OB-BKG-1", Shipper: "cymbal-shoes.example", Vessel: "Ocean Blue Aurora", Voyage: "42W", Status: "in transit"}}, nil
}
func (n *carrierNode) BookingRecords(context.Context, string) (*operator.BookingRecordsResponse, error) {
	return &operator.BookingRecordsResponse{}, nil
}
func (n *carrierNode) Disrupt(_ context.Context, req operator.DisruptRequest) (*operator.DisruptResponse, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.disrupted = append(n.disrupted, req)
	return &operator.DisruptResponse{Alerts: []operator.VoyageAlert{{BookingRef: "OB-BKG-1", Alert: &intents.TnTAlertBody{AlertID: "AL-1",
		Kind: intents.AlertDelay, Severity: intents.SeverityHigh, Summary: req.Reason, PreviousETA: "a", NewETA: "b", CauseEventIDs: []string{"EV-1"}}}}}, nil
}

// bothSidesUI is a UI with an assistant on each side: the shipper's and
// the carrier's.
func bothSidesUI(t *testing.T) (http.Handler, *carrierNode, *time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	cn := &carrierNode{}
	h := Handler(Config{Company: "Ocean Blue", Domain: "ocean-blue.example", Personas: personas, Control: &echo{}, Now: clock,
		AssistantSides: []string{SideCustomer, SideOperator},
		NewAssistant: func(side string) (*assistant.Assistant, error) {
			if side == SideOperator {
				return &assistant.Assistant{Model: assistant.FakeCarrierModel(), Carrier: cn, Drafts: &assistant.Drafts{}, Now: clock,
					Self: assistant.Self{Domain: "ocean-blue.example", Name: "Ocean Blue"}}, nil
			}
			return &assistant.Assistant{Model: assistant.FakeModel(clock), Node: &node{}, Drafts: &assistant.Drafts{}, Now: clock,
				Self: assistant.Self{Domain: "ocean-blue.example", Name: "Ocean Blue"}}, nil
		}})
	return h, cn, &now
}

// TestCarrierAssistant: the operator side has its own assistant, for the
// operator role only; it drafts a delay, and only the dialog's send, once
// armed, publishes it.
func TestCarrierAssistant(t *testing.T) {
	h, cn, now := bothSidesUI(t)
	alex, sam, jo := login(t, h, "alex"), login(t, h, "sam"), login(t, h, "jo")
	for name, c := range map[string]struct {
		cookie *http.Cookie
		want   string
	}{"alex (shipper)": {alex, "customer"}, "sam (carrier)": {sam, "operator"}, "jo (both)": {jo, "customer,operator"}} {
		var me MeResponse
		_ = json.Unmarshal(do(h, http.MethodGet, "/ui/api/me", "", c.cookie).Body.Bytes(), &me)
		if strings.Join(me.Assistants, ",") != c.want {
			t.Errorf("%s: assistants %v", name, me.Assistants)
		}
	}
	// The operator side is the operator role's, and the customer side the
	// shipper role's.
	if code, _ := post(h, "/ui/api/assistant/turn?side=operator", map[string]string{"text": "hi"}, alex); code != http.StatusForbidden {
		t.Errorf("a shipper on the carrier's assistant: HTTP %d", code)
	}
	if rec := do(h, http.MethodGet, "/ui/api/assistant/progress?side=operator", "", alex); rec.Code != http.StatusForbidden {
		t.Errorf("a shipper reading the carrier's progress: HTTP %d", rec.Code)
	}
	// Only side=operator is the operator side; anything else is the
	// customer side, and needs the shipper role.
	for _, q := range []string{"?side=Operator", "?side=", "?side=customer&side=operator", ""} {
		if code, _ := post(h, "/ui/api/assistant/turn"+q, map[string]string{"text": "hi"}, sam); code != http.StatusForbidden {
			t.Errorf("sam on %q: HTTP %d", q, code)
		}
	}
	code, st := post(h, "/ui/api/assistant/turn?side=operator", map[string]string{"text": "Delay voyage 42W by 3 days: engine repair"}, sam)
	if code != http.StatusOK || len(st.Drafts) != 1 || st.Drafts[0].Disruption == nil || st.Drafts[0].DraftID != "DD-0001" {
		t.Fatalf("draft: HTTP %d %+v", code, st)
	}
	if len(cn.disrupted) != 0 {
		t.Fatal("a draft published a delay")
	}
	// Too early, then from the customer side (no such draft there), then
	// armed: published once.
	if code, _ := post(h, "/ui/api/assistant/send?side=operator", SendRequest{DraftID: "DD-0001"}, sam); code != http.StatusTooEarly {
		t.Errorf("an immediate send: HTTP %d", code)
	}
	*now = now.Add(ArmDelay)
	if code, _ := post(h, "/ui/api/assistant/send", SendRequest{DraftID: "DD-0001"}, jo); code != http.StatusNotFound {
		t.Errorf("a delay sent from the customer side: HTTP %d", code)
	}
	code, st = post(h, "/ui/api/assistant/send?side=operator", SendRequest{DraftID: "DD-0001"}, sam)
	if code != http.StatusOK || len(cn.disrupted) != 1 || cn.disrupted[0].Voyage != "42W" || cn.disrupted[0].Delay != "72h0m0s" {
		t.Fatalf("send: HTTP %d, %+v", code, cn.disrupted)
	}
	if note := st.Lines[len(st.Lines)-2].Text; note != "Published DD-0001: signed alerts for OB-BKG-1." {
		t.Errorf("the transcript's note: %q", note)
	}
	// A booking draft's ID on the operator side: no such draft there.
	if code, _ := post(h, "/ui/api/assistant/send?side=operator", SendRequest{DraftID: "BD-0001"}, sam); code != http.StatusNotFound {
		t.Errorf("a booking draft through the operator side: HTTP %d", code)
	}
	// Jo has a conversation on each side, apart.
	_, cs := post(h, "/ui/api/assistant/turn", map[string]string{"text": "hello"}, jo)
	_, os := post(h, "/ui/api/assistant/turn?side=operator", map[string]string{"text": "hello"}, jo)
	if len(cs.Lines) != 2 || len(os.Lines) != 2 || !strings.Contains(os.Lines[1].Text, "prepare a delay") || strings.Contains(cs.Lines[1].Text, "prepare a delay") {
		t.Errorf("jo's two conversations: %+v / %+v", cs.Lines, os.Lines)
	}
}

func TestSentOutcome(t *testing.T) {
	alert := &intents.TnTAlertBody{AlertID: "AL-9", PreviousETA: "a", NewETA: "b"}
	for want, s := range map[string]*assistant.Sent{
		"Published DD-0002: alert AL-9, ETA a → b.": {Draft: assistant.Draft{ID: "DD-0002"}, Disrupted: &operator.DisruptResponse{Alert: alert}},
		"Published DD-0005: the delay is applied, but its alert was not sent (don't publish it again).": {Draft: assistant.Draft{ID: "DD-0005"},
			Disrupted: &operator.DisruptResponse{Applied: true}},
		"Published DD-0003: no alerts went out. OB-1 not delayed.": {Draft: assistant.Draft{ID: "DD-0003"},
			Disrupted: &operator.DisruptResponse{Alerts: []operator.VoyageAlert{{BookingRef: "OB-1"}}}},
		"Published DD-0004: signed alerts for OB-1. OB-2 DELAYED, alert not sent (don't publish again).": {Draft: assistant.Draft{ID: "DD-0004"},
			Disrupted: &operator.DisruptResponse{Alerts: []operator.VoyageAlert{{BookingRef: "OB-1", Alert: alert}, {BookingRef: "OB-2", Applied: true}}}},
		"Sent BD-0001 to ocean-blue.example, signed. Answer: confirmed OB-BKG-1": {Draft: assistant.Draft{ID: "BD-0001", Operator: "ocean-blue.example"},
			Confirmation: &intents.BookingConfirmationBody{Status: "confirmed", BookingRef: "OB-BKG-1"}},
	} {
		if got := sentOutcome(s); got != want {
			t.Errorf("%q, want %q", got, want)
		}
	}
}
