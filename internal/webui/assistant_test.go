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
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/assistant"
	"github.com/go-steer/supercargo/internal/llm"
	"github.com/go-steer/supercargo/internal/llm/fake"
	"github.com/go-steer/supercargo/internal/shipper"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/intents"
)

// node is the shipper's node as the assistant sees it; it counts bookings.
type node struct {
	mu     sync.Mutex
	booked []shipper.BookRequest
	fail   error         // Book fails with it, if set
	hold   chan struct{} // Book waits for it to close, if set
	// lookup, if set, gets a value when Profile starts, then Profile waits
	// for it to close.
	lookup chan struct{}
}

func (n *node) Profile(context.Context, string) (*shipper.Profile, error) {
	n.mu.Lock()
	lookup := n.lookup
	n.lookup = nil // once
	n.mu.Unlock()
	if lookup != nil {
		lookup <- struct{}{}
		<-lookup
	}
	return &shipper.Profile{Domain: "ocean-blue.example", Name: "Ocean Blue", Intents: []string{intents.BookingRequest}}, nil
}
func (n *node) Shipments(context.Context) ([]store.Summary, error) { return nil, nil }
func (n *node) Records(context.Context, string, string) (*shipper.RecordsResponse, error) {
	return nil, errors.New("no")
}
func (n *node) Ask(context.Context, string, string, string) (*shipper.AskResponse, error) {
	return nil, errors.New("no")
}
func (n *node) Status(context.Context, string, string) (*shipper.StatusResponse, error) {
	return nil, errors.New("no")
}
func (n *node) Book(_ context.Context, req shipper.BookRequest) (*shipper.BookResponse, error) {
	if n.hold != nil {
		<-n.hold
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.fail != nil {
		return nil, n.fail
	}
	n.booked = append(n.booked, req)
	return &shipper.BookResponse{Confirmation: &intents.BookingConfirmationBody{BookingRef: "OB-BKG-1", Status: intents.StatusConfirmed,
		PlannedArrival: "2026-10-13T08:00:00Z"}}, nil
}

const bookSentence = "Book one 40ft high-cube of sneakers for CY-PO-55120, Rotterdam to Newark, departing 2026-10-06, with ocean-blue.example."

func assistantUI(t *testing.T) (http.Handler, *node, *echo, *time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	n, ctl := &node{}, &echo{}
	h := Handler(Config{Company: "Cymbal Shoes", Domain: "cymbal-shoes.example", Personas: personas, Control: ctl, Now: clock,
		NewAssistant: func() (*assistant.Assistant, error) {
			return &assistant.Assistant{Model: assistant.FakeModel(clock), Node: n, Drafts: &assistant.Drafts{}, Now: clock,
				Self: assistant.Self{Domain: "cymbal-shoes.example", Name: "Cymbal Shoes"}}, nil
		}})
	return h, n, ctl, &now
}

func post(h http.Handler, path string, v any, c *http.Cookie, hdr ...string) (int, AssistantResponse) {
	b, _ := json.Marshal(v)
	rec := do(h, http.MethodPost, path, string(b), c, hdr...)
	var r AssistantResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &r)
	return rec.Code, r
}

// TestBookingNeedsAPersonsYes: the model drafts; only the send route,
// once the dialog has been up ArmDelay, books, and only once.
func TestBookingNeedsAPersonsYes(t *testing.T) {
	h, n, _, now := assistantUI(t)
	alex := login(t, h, "alex")
	var me MeResponse
	_ = json.Unmarshal(do(h, http.MethodGet, "/ui/api/me", "", alex).Body.Bytes(), &me)
	if !me.Assistant {
		t.Fatal("the shipper persona has no assistant")
	}
	code, st := post(h, "/ui/api/assistant/turn", map[string]string{"text": bookSentence}, alex)
	if code != http.StatusOK || len(st.Drafts) != 1 || len(st.Lines) != 2 || len(st.Lines[1].Tools) != 1 || st.Lines[1].Tools[0].Name != assistant.ToolDraftBooking {
		t.Fatalf("turn: HTTP %d %+v", code, st)
	}
	id := st.Drafts[0].DraftID
	if len(n.booked) != 0 {
		t.Fatal("a draft booked by itself")
	}
	// At once: too early, nothing sent.
	if code, _ := post(h, "/ui/api/assistant/send", SendRequest{DraftID: id}, alex); code != http.StatusTooEarly || len(n.booked) != 0 {
		t.Errorf("an immediate send: HTTP %d, %d booked", code, len(n.booked))
	}
	// From another site: refused.
	*now = now.Add(ArmDelay)
	if code, _ := post(h, "/ui/api/assistant/send", SendRequest{DraftID: id}, alex, "Origin", "https://evil.example"); code != http.StatusForbidden {
		t.Errorf("a cross-site send: HTTP %d", code)
	}
	// Another session can't send this session's draft.
	other := login(t, h, "jo")
	if code, _ := post(h, "/ui/api/assistant/send", SendRequest{DraftID: id}, other); code != http.StatusNotFound || len(n.booked) != 0 {
		t.Errorf("another session's send: HTTP %d, %d booked", code, len(n.booked))
	}
	code, st = post(h, "/ui/api/assistant/send", SendRequest{DraftID: id}, alex)
	if code != http.StatusOK || len(n.booked) != 1 || len(st.Drafts) != 0 || !strings.Contains(st.Lines[len(st.Lines)-1].Text, "Booked") {
		t.Fatalf("send: HTTP %d, %d booked, %+v", code, len(n.booked), st)
	}
	if n.booked[0].Operator != "ocean-blue.example" || n.booked[0].Request.ShipperRef != "CY-PO-55120" {
		t.Errorf("booked %+v", n.booked[0])
	}
	if code, _ := post(h, "/ui/api/assistant/send", SendRequest{DraftID: id}, alex); code != http.StatusNotFound || len(n.booked) != 1 {
		t.Errorf("a second send: HTTP %d, %d booked", code, len(n.booked))
	}
	// The transcript survives a reload.
	var again AssistantResponse
	_ = json.Unmarshal(do(h, http.MethodGet, "/ui/api/assistant", "", alex).Body.Bytes(), &again)
	if len(again.Lines) != len(st.Lines) {
		t.Errorf("reloaded transcript: %d lines, want %d", len(again.Lines), len(st.Lines))
	}
}

func TestDiscard(t *testing.T) {
	h, n, _, now := assistantUI(t)
	alex := login(t, h, "alex")
	_, st := post(h, "/ui/api/assistant/turn", map[string]string{"text": bookSentence}, alex)
	id := st.Drafts[0].DraftID
	if code, st := post(h, "/ui/api/assistant/discard", SendRequest{DraftID: id}, alex); code != http.StatusOK || len(st.Drafts) != 0 {
		t.Errorf("discard: HTTP %d %+v", code, st)
	}
	*now = now.Add(ArmDelay)
	if code, _ := post(h, "/ui/api/assistant/send", SendRequest{DraftID: id}, alex); code != http.StatusNotFound || len(n.booked) != 0 {
		t.Errorf("a discarded draft was sent: HTTP %d", code)
	}
	if code, _ := post(h, "/ui/api/assistant/discard", SendRequest{DraftID: id}, alex); code != http.StatusNotFound {
		t.Errorf("a second discard: HTTP %d", code)
	}
}

// TestProgress: while a turn runs, the page sees the calls made so far
// and the one running, with its arguments in short; another session sees
// nothing of it, and once the turn is over there's nothing to show.
func TestProgress(t *testing.T) {
	h, n, _, _ := assistantUI(t)
	alex, jo := login(t, h, "alex"), login(t, h, "jo")
	progress := func(c *http.Cookie) Progress {
		var p Progress
		rec := do(h, http.MethodGet, "/ui/api/assistant/progress", "", c)
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &p) != nil {
			t.Fatalf("progress: HTTP %d %s", rec.Code, rec.Body)
		}
		return p
	}
	if p := progress(alex); p.Working || len(p.Tools) != 0 || p.Running != nil {
		t.Errorf("idle: %+v", p)
	}
	lookup := make(chan struct{})
	n.mu.Lock()
	n.lookup = lookup
	n.mu.Unlock()
	done := make(chan AssistantResponse)
	go func() {
		_, st := post(h, "/ui/api/assistant/turn", map[string]string{"text": "We have a new carrier, ocean-blue.example. What can they do?"}, alex)
		done <- st
	}()
	<-lookup // the profile lookup has started
	// The progress carries no drafts (or anything but these), so polling
	// it can never start a draft's ArmDelay.
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(do(h, http.MethodGet, "/ui/api/assistant/progress", "", alex).Body.Bytes(), &raw)
	for k := range raw {
		if k != "working" && k != "tools" && k != "running" {
			t.Errorf("the progress carries %q", k)
		}
	}
	p := progress(alex)
	if !p.Working || p.Running == nil || p.Running.Name != assistant.ToolGetProfile || p.Running.Detail != "ocean-blue.example" {
		t.Errorf("mid-turn: %+v", p)
	}
	if p := progress(jo); p.Working || p.Running != nil {
		t.Errorf("another session saw this one's turn: %+v", p)
	}
	close(lookup)
	st := <-done
	last := st.Lines[len(st.Lines)-1]
	if len(last.Tools) != 1 || last.Tools[0].Detail != "ocean-blue.example" {
		t.Errorf("the transcript's tool line: %+v", last.Tools)
	}
	if p := progress(alex); p.Working || len(p.Tools) != 0 || p.Running != nil {
		t.Errorf("after the turn: %+v", p)
	}
}

func TestToolDetail(t *testing.T) {
	for in, want := range map[string]string{
		`{"bookingRef":"OB-1","question":"Why?"}`:     "OB-1, Why?",
		`{"count":2,"reefer":false,"nested":{"x":1}}`: "2, false",
		`{"q":"line one\nline two\u202e"}`:            "line one line two",
		`[1,2]`:                                       "",
		`{"q":"` + strings.Repeat("x", 200) + `"}`:    strings.Repeat("x", 79) + "…",
		`{"a":"x",`: "x",
	} {
		if got := toolDetail(json.RawMessage(in)); got != want {
			t.Errorf("%s: %q, want %q", in, got, want)
		}
	}
}

// TestAssistantGates: the assistant is the shipper role's, behind a
// session, same-origin and JSON; disrupting is the carrier role's.
func TestAssistantGates(t *testing.T) {
	h, _, ctl, _ := assistantUI(t)
	alex, sam := login(t, h, "alex"), login(t, h, "sam")
	for _, path := range []string{"/ui/api/assistant/turn", "/ui/api/assistant/send", "/ui/api/assistant/discard"} {
		if code, _ := post(h, path, map[string]string{"text": "hi"}, nil); code != http.StatusUnauthorized {
			t.Errorf("%s without a session: HTTP %d", path, code)
		}
		if code, _ := post(h, path, map[string]string{"text": "hi"}, sam); code != http.StatusForbidden {
			t.Errorf("%s as the carrier: HTTP %d", path, code)
		}
	}
	for _, path := range []string{"/ui/api/assistant", "/ui/api/assistant/progress"} {
		if rec := do(h, http.MethodGet, path, "", sam); rec.Code != http.StatusForbidden {
			t.Errorf("GET %s as the carrier: HTTP %d", path, rec.Code)
		}
		if rec := do(h, http.MethodGet, path, "", nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s with no session: HTTP %d", path, rec.Code)
		}
	}
	if code, _ := post(h, "/ui/api/assistant/turn", map[string]string{"text": "hi"}, alex, "Origin", "https://evil.example"); code != http.StatusForbidden {
		t.Errorf("a cross-site turn: HTTP %d", code)
	}
	if rec := do(h, http.MethodPost, "/ui/api/assistant/turn", "text=hi", alex, "Content-Type", "text/plain"); rec.Code != http.StatusForbidden {
		t.Errorf("a non-JSON turn: HTTP %d", rec.Code)
	}
	if code, st := post(h, "/ui/api/assistant/turn", map[string]string{"text": strings.Repeat("x", assistant.MaxUserText+1)}, alex); code != http.StatusBadRequest {
		t.Errorf("an overlong message: HTTP %d %s", code, st.Error)
	}

	disrupt := map[string]string{"bookingRef": "OB-BKG-1", "delay": "72h", "reason": "Engine repair."}
	if code, _ := post(h, "/ui/api/disrupt", disrupt, alex); code != http.StatusForbidden {
		t.Errorf("a shipper disrupting: HTTP %d", code)
	}
	if code, _ := post(h, "/ui/api/disrupt", disrupt, sam, "Origin", "https://evil.example"); code != http.StatusForbidden {
		t.Errorf("a cross-site disrupt: HTTP %d", code)
	}
	if code, _ := post(h, "/ui/api/disrupt", map[string]string{"bookingRef": "x", "extra": "y"}, sam); code != http.StatusBadRequest {
		t.Errorf("a disrupt with an unknown member: HTTP %d", code)
	}
	ctl.got = nil
	if code, _ := post(h, "/ui/api/disrupt", disrupt, sam); code != http.StatusOK || ctl.got == nil || ctl.got.Method != http.MethodPost ||
		ctl.got.URL.Path != "/disrupt" || ctl.got.Header.Get("Cookie") != "" {
		t.Errorf("disrupt: HTTP %d, control saw %+v", code, ctl.got)
	}
	// A node without an assistant says so, and has none to drive.
	plain, _, _ := newUI(t)
	c := login(t, plain, "alex")
	var me MeResponse
	_ = json.Unmarshal(do(plain, http.MethodGet, "/ui/api/me", "", c).Body.Bytes(), &me)
	if me.Assistant {
		t.Error("a node without an assistant offered one")
	}
	if code, _ := post(plain, "/ui/api/assistant/turn", map[string]string{"text": "hi"}, c); code != http.StatusConflict {
		t.Errorf("a turn with no assistant: HTTP %d", code)
	}
}

// TestSendFailures: a booking the node refused before sending stays a
// draft (it can be fixed and sent); one whose fate is unknown is gone, so
// a second yes can't book twice. Either way the transcript says so.
func TestSendFailures(t *testing.T) {
	h, n, _, now := assistantUI(t)
	alex := login(t, h, "alex")
	_, st := post(h, "/ui/api/assistant/turn", map[string]string{"text": bookSentence}, alex)
	id := st.Drafts[0].DraftID
	*now = now.Add(ArmDelay)
	n.fail = &assistant.NodeError{Status: http.StatusBadRequest}
	code, st := post(h, "/ui/api/assistant/send", SendRequest{DraftID: id}, alex)
	if code != http.StatusBadGateway || len(st.Drafts) != 1 || st.Error == "" || !strings.Contains(st.Lines[len(st.Lines)-1].Text, "not sent") {
		t.Errorf("a refused send: HTTP %d %+v", code, st)
	}
	n.fail = errors.New("connection reset")
	code, st = post(h, "/ui/api/assistant/send", SendRequest{DraftID: id}, alex)
	if code != http.StatusBadGateway || len(st.Drafts) != 0 {
		t.Errorf("a send of unknown outcome: HTTP %d, drafts %+v", code, st.Drafts)
	}
	n.fail = nil
	if code, _ := post(h, "/ui/api/assistant/send", SendRequest{DraftID: id}, alex); code != http.StatusNotFound || len(n.booked) != 0 {
		t.Errorf("a second yes after an unknown outcome: HTTP %d, %d booked", code, len(n.booked))
	}
}

func TestAssistantUnavailable(t *testing.T) {
	broken := Handler(Config{Domain: "cymbal-shoes.example", Personas: personas, Control: &echo{},
		NewAssistant: func() (*assistant.Assistant, error) { return nil, errors.New("no model configured") }})
	c := login(t, broken, "alex")
	if rec := do(broken, http.MethodGet, "/ui/api/assistant", "", c); rec.Code != http.StatusInternalServerError {
		t.Errorf("state with a broken assistant: HTTP %d", rec.Code)
	}
	if code, st := post(broken, "/ui/api/assistant/turn", map[string]string{"text": "hi"}, c); code != http.StatusConflict || !strings.Contains(st.Error, "no model") {
		t.Errorf("a turn with a broken assistant: HTTP %d %+v", code, st)
	}
	plain, _, _ := newUI(t)
	var st AssistantResponse
	rec := do(plain, http.MethodGet, "/ui/api/assistant", "", login(t, plain, "alex"))
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &st) != nil || st.Available {
		t.Errorf("state with no assistant: HTTP %d %+v", rec.Code, st)
	}
	if code, _ := post(plain, "/ui/api/assistant/turn", "not an object", login(t, plain, "alex")); code != http.StatusBadRequest {
		t.Errorf("a malformed turn: HTTP %d", code)
	}
}

// TestTranscriptBounded: a session's transcript keeps the last
// MaxTranscript lines.
func TestTranscriptBounded(t *testing.T) {
	h, _, _, _ := assistantUI(t)
	alex := login(t, h, "alex")
	var st AssistantResponse
	for range MaxTranscript/2 + 3 {
		_, st = post(h, "/ui/api/assistant/turn", map[string]string{"text": "Hello"}, alex)
	}
	if len(st.Lines) != MaxTranscript || st.Lines[len(st.Lines)-1].Who != "assistant" {
		t.Errorf("transcript: %d lines", len(st.Lines))
	}
}

// TestArmFromShown: the delay runs from when the node handed the draft to
// the page, not from when the model made it.
func TestArmFromShown(t *testing.T) {
	h, n, _, now := assistantUI(t)
	alex := login(t, h, "alex")
	_, st := post(h, "/ui/api/assistant/turn", map[string]string{"text": bookSentence}, alex)
	id := st.Drafts[0].DraftID
	*now = now.Add(ArmDelay - time.Millisecond)
	if code, _ := post(h, "/ui/api/assistant/send", SendRequest{DraftID: id}, alex); code != http.StatusTooEarly || len(n.booked) != 0 {
		t.Errorf("just under the delay: HTTP %d", code)
	}
	// Reading the state again doesn't restart (or skip) the clock.
	do(h, http.MethodGet, "/ui/api/assistant", "", alex)
	*now = now.Add(time.Millisecond)
	if code, _ := post(h, "/ui/api/assistant/send", SendRequest{DraftID: id}, alex); code != http.StatusOK || len(n.booked) != 1 {
		t.Errorf("at the delay: HTTP %d", code)
	}
}

// TestConcurrentSends: two sends of one draft book once; while one runs,
// the session is busy for turns too; the loser says nothing false.
func TestConcurrentSends(t *testing.T) {
	h, n, _, now := assistantUI(t)
	alex := login(t, h, "alex")
	_, st := post(h, "/ui/api/assistant/turn", map[string]string{"text": bookSentence}, alex)
	id := st.Drafts[0].DraftID
	*now = now.Add(ArmDelay)
	n.hold = make(chan struct{})
	first := make(chan int, 1)
	go func() { code, _ := post(h, "/ui/api/assistant/send", SendRequest{DraftID: id}, alex); first <- code }()
	time.Sleep(50 * time.Millisecond)
	code, second := post(h, "/ui/api/assistant/send", SendRequest{DraftID: id}, alex)
	if code != http.StatusConflict {
		t.Errorf("a second send while the first runs: HTTP %d", code)
	}
	if code, _ := post(h, "/ui/api/assistant/turn", map[string]string{"text": "Hello"}, alex); code != http.StatusConflict {
		t.Errorf("a turn while a send runs: HTTP %d", code)
	}
	close(n.hold)
	if c := <-first; c != http.StatusOK || len(n.booked) != 1 {
		t.Fatalf("the first send: HTTP %d, %d booked", c, len(n.booked))
	}
	code, st = post(h, "/ui/api/assistant/send", SendRequest{DraftID: id}, alex)
	if code != http.StatusNotFound || len(n.booked) != 1 {
		t.Errorf("a send after it: HTTP %d, %d booked", code, len(n.booked))
	}
	for _, l := range append(st.Lines, second.Lines...) {
		if strings.Contains(l.Text, "not sent") {
			t.Errorf("a false note: %q", l.Text)
		}
	}
}

// TestChatEndsWithSession: signing out drops the assistant and its
// drafts; a two-role persona has both the assistant and disrupt.
func TestChatEndsWithSession(t *testing.T) {
	h, _, _, _ := assistantUI(t)
	alex := login(t, h, "alex")
	post(h, "/ui/api/assistant/turn", map[string]string{"text": bookSentence}, alex)
	do(h, http.MethodPost, "/ui/api/logout", "{}", alex)
	alex = login(t, h, "alex")
	var st AssistantResponse
	_ = json.Unmarshal(do(h, http.MethodGet, "/ui/api/assistant", "", alex).Body.Bytes(), &st)
	if len(st.Lines) != 0 || len(st.Drafts) != 0 {
		t.Errorf("a new session inherited the old one's chat: %+v", st)
	}
	jo := login(t, h, "jo")
	if code, _ := post(h, "/ui/api/assistant/turn", map[string]string{"text": "Hello"}, jo); code != http.StatusOK {
		t.Errorf("a two-role persona's turn: HTTP %d", code)
	}
	if code, _ := post(h, "/ui/api/disrupt", map[string]string{"bookingRef": "OB-1", "delay": "24h", "reason": "Fog."}, jo); code != http.StatusOK {
		t.Errorf("a two-role persona's disrupt: HTTP %d", code)
	}
}

// TestTurnLimits: a failing model leaves a note, not a lost message; when
// every turn slot is taken, a turn waits for none of them.
func TestTurnLimits(t *testing.T) {
	failing := Handler(Config{Domain: "cymbal-shoes.example", Personas: personas, Control: &echo{},
		NewAssistant: func() (*assistant.Assistant, error) {
			return &assistant.Assistant{Model: &fake.Model{Respond: func(llm.GenerateRequest) (llm.GenerateResponse, error) {
				return llm.GenerateResponse{}, errors.New("model unavailable")
			}}, Node: &node{}, Drafts: &assistant.Drafts{}}, nil
		}})
	c := login(t, failing, "alex")
	code, st := post(failing, "/ui/api/assistant/turn", map[string]string{"text": "Hello"}, c)
	if code != http.StatusBadGateway || len(st.Lines) != 2 || st.Lines[0].Text != "Hello" || st.Lines[1].Who != "note" {
		t.Errorf("a failed turn: HTTP %d %+v", code, st.Lines)
	}
	h, _, _, _ := assistantUI(t)
	alex := login(t, h, "alex")
	for range MaxTurnsAtOnce {
		turnSlots <- struct{}{}
	}
	code, _ = post(h, "/ui/api/assistant/turn", map[string]string{"text": "Hello"}, alex)
	for range MaxTurnsAtOnce {
		<-turnSlots
	}
	if code != http.StatusServiceUnavailable {
		t.Errorf("a turn with every slot taken: HTTP %d", code)
	}
	if code, _ := post(h, "/ui/api/assistant/turn", map[string]string{"text": "Hello"}, alex); code != http.StatusOK {
		t.Errorf("a turn once a slot is free (the session's lock was released): HTTP %d", code)
	}
}
