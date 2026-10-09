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

package app

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/activity"
	"github.com/go-steer/supercargo/internal/assistant"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/internal/webui"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
	"github.com/go-steer/supercargo/pkg/sig"
)

// messages lists a's message log through its control API.
func messages(t *testing.T, a *App, query string) []store.Message {
	t.Helper()
	var r MessagesResponse
	if code := controlGet(t, a, "/messages"+query, &r); code != http.StatusOK {
		t.Fatalf("/messages%s: HTTP %d %s", query, code, r.Error)
	}
	return r.Messages
}

func find(ms []store.Message, dir, intent string) *store.Message {
	for i := range ms {
		if ms[i].Direction == dir && ms[i].Intent == intent {
			return &ms[i]
		}
	}
	return nil
}

// TestMessageLog: each node keeps exactly the bytes it sent and received,
// its side of the conversation only; the shipper's copy of the
// confirmation is byte for byte the operator's.
func TestMessageLog(t *testing.T) {
	p := startPairWith(t, "", `role: shipper
control: { socket: "`+sockDir(t)+`/cy.sock" }`)
	conf, err := p.shipper.Shipper.Book(context.Background(), "ocean-blue.example", request())
	if err != nil {
		t.Fatal(err)
	}
	cy := messages(t, p.shipper, "")
	ob := p.operator.Messages.List(store.MessageFilter{})
	sentReq, gotConf := find(cy, "out", intents.BookingRequest), find(cy, "in", intents.BookingConfirmation)
	gotReq, sentConf := find(ob, "in", intents.BookingRequest), find(ob, "out", intents.BookingConfirmation)
	if sentReq == nil || gotConf == nil || gotReq == nil || sentConf == nil {
		t.Fatalf("missing messages: shipper %+v, operator %+v", cy, ob)
	}
	if sentReq.Wire != gotReq.Wire || gotConf.Wire != sentConf.Wire {
		t.Error("a node's copy of a message isn't the bytes the other one has")
	}
	for _, m := range []*store.Message{sentReq, gotConf} {
		if m.Side != activity.SideCustomer || !m.Verified || m.Header == nil || m.Conversation != sentReq.Header.CorrelationID || m.Body == nil {
			t.Errorf("shipper's %s %s: %+v", m.Direction, m.Intent, m)
		}
	}
	if gotReq.Side != activity.SideOperator || sentConf.Side != activity.SideOperator {
		t.Errorf("operator sides: %q, %q", gotReq.Side, sentConf.Side)
	}
	// The body shown is the verified payload's: the eBL as the operator
	// signed it.
	var body intents.BookingConfirmationBody
	if err := json.Unmarshal(gotConf.Body, &body); err != nil || body.BookingRef != conf.BookingRef {
		t.Errorf("confirmation body %s: %v", gotConf.Body, err)
	}
	if u, err := envelope.Parse([]byte(gotConf.Wire)); err != nil || u.ClaimedHeader().MessageID != gotConf.MessageID {
		t.Errorf("the wire doesn't parse as the message: %v", err)
	}
	// One message by number; the other side's view doesn't find it.
	var one MessageResponse
	path := "/messages/" + strconv.FormatUint(gotConf.Seq, 10)
	if code := controlGet(t, p.shipper, path+"?side=customer", &one); code != http.StatusOK || one.Message.Wire != gotConf.Wire {
		t.Errorf("%s: HTTP %d", path, code)
	}
	if code := controlGet(t, p.shipper, path+"?side=operator", &one); code != http.StatusNotFound {
		t.Errorf("%s as the operator side: HTTP %d", path, code)
	}
	if ms := messages(t, p.shipper, "?conversation="+sentReq.Conversation+"&limit=1"); len(ms) != 1 || ms[0].Intent != intents.BookingConfirmation {
		t.Errorf("the newest message of the conversation: %+v", ms)
	}
	// Nothing of the message reaches the activity stream (or the log).
	past, _, _, cancel := p.shipper.Activity.Subscribe(0, 0)
	cancel()
	for _, e := range past {
		if b, _ := json.Marshal(e); bytes.Contains(b, []byte(`"wire"`)) || bytes.Contains(b, []byte("shipper_reference")) {
			t.Errorf("an activity entry carries message content: %s", b)
		}
	}
}

// TestMessageLogRefused: messages the node refused are kept too, as text,
// never with a body or as verified, and malformed ones belong to the
// whole node.
func TestMessageLogRefused(t *testing.T) {
	p := startPair(t, "")
	post := func(b []byte) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/ulp", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		p.operator.Node.Handler().ServeHTTP(rec, req)
	}
	post([]byte("<script>alert(1)</script>"))
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	forger, _ := sig.NewEd25519Signer(priv)
	body, _ := json.Marshal(request())
	forged, err := envelope.Seal(&envelope.Envelope{Header: intents.HeaderFor(intents.BookingRequest, "cymbal-shoes.example", "ocean-blue.example", time.Now()),
		Body: body}, "cy-1", forger)
	if err != nil {
		t.Fatal(err)
	}
	post(forged)
	ms := p.operator.Messages.List(store.MessageFilter{})
	if len(ms) != 2 {
		t.Fatalf("%d messages kept: %+v", len(ms), ms)
	}
	if m := ms[0]; m.Outcome != "malformed" || m.Side != "" || m.Header != nil || m.Body != nil || !strings.Contains(m.Wire, "<script>") {
		t.Errorf("malformed: %+v", m)
	}
	if m := ms[1]; m.Outcome != "rejected" || m.Verified || m.Body != nil || m.Header == nil || m.Header.Sender != "cymbal-shoes.example" ||
		m.Side != activity.SideOperator || m.Wire != string(forged) {
		t.Errorf("forged: %+v", m)
	}
	// JSON escapes the markup: a reader that shows it as text is safe.
	b, _ := json.Marshal(ms[0])
	if bytes.Contains(b, []byte("<script>")) {
		t.Errorf("markup unescaped in JSON: %s", b)
	}
}

// TestMessageSidesOnTwoRoleNode: on a carrier that also ships, each
// message is on the side of the role that took part, and each side's
// view leaves the other's out.
func TestMessageSidesOnTwoRoleNode(t *testing.T) {
	p := startPairWith(t, "", `roles: [shipper, operator]
control: { socket: "`+sockDir(t)+`/cy.sock" }
operator: { refPrefix: CY-BKG, containerPrefix: CYMU, vessel: Sneaker, voyage: 1E, departureHour: 6, transit: 48h }`)
	if _, err := p.shipper.Shipper.Book(context.Background(), "ocean-blue.example", request()); err != nil {
		t.Fatal(err)
	}
	h := intents.HeaderFor(intents.BookingRequest, "ocean-blue.example", "cymbal-shoes.example", time.Now())
	req := request()
	req.ShipperName = "Ocean Blue"
	if _, err := p.operator.Node.Exchange(context.Background(), h, req, intents.BookingConfirmation); err != nil {
		t.Fatal(err)
	}
	customer, operator := messages(t, p.shipper, "?side=customer"), messages(t, p.shipper, "?side=operator")
	if find(customer, "out", intents.BookingRequest) == nil || find(customer, "in", intents.BookingRequest) != nil {
		t.Errorf("customer side: %+v", customer)
	}
	if find(operator, "in", intents.BookingRequest) == nil || find(operator, "out", intents.BookingRequest) != nil {
		t.Errorf("operator side: %+v", operator)
	}
	var r MessagesResponse
	if code := controlGet(t, p.shipper, "/messages?side=everyone", &r); code != http.StatusBadRequest {
		t.Errorf("an unknown side: HTTP %d", code)
	}
	for _, q := range []string{"?since=x", "?limit=0", "?limit=501"} {
		if code := controlGet(t, p.shipper, "/messages"+q, &r); code != http.StatusBadRequest {
			t.Errorf("%s: HTTP %d", q, code)
		}
	}
	var one MessageResponse
	if code := controlGet(t, p.shipper, "/messages/x", &one); code != http.StatusBadRequest {
		t.Errorf("/messages/x: HTTP %d", code)
	}
	if code := controlGet(t, p.shipper, "/messages/1?side=nobody", &one); code != http.StatusBadRequest {
		t.Errorf("/messages/1?side=nobody: HTTP %d", code)
	}
}

// TestSideOf: every intent and direction lands on the side of the role
// that took part, and a side the node doesn't have is the whole node's.
func TestSideOf(t *testing.T) {
	both := &App{Carrier: newRecords(), Customer: newRecords()}
	carrier := &App{Carrier: newRecords()}
	shipper := &App{Customer: newRecords()}
	const op, cu = activity.SideOperator, activity.SideCustomer
	for _, tc := range []struct {
		a          *App
		opener     string
		openedHere bool
		want       string
	}{
		// A request we sent, and its reply (or ulp.error): customer.
		{both, intents.BookingRequest, true, cu},
		{both, intents.TnTInquiry, true, cu},
		{both, intents.OceanTnTRequest, true, cu},
		// A request we received, and our reply: operator.
		{both, intents.BookingRequest, false, op},
		{both, intents.TnTInquiry, false, op},
		{both, intents.OceanTnTRequest, false, op},
		// A push we sent: operator; one we received: customer.
		{both, intents.TnTEvent, true, op},
		{both, intents.TnTAlert, true, op},
		{both, intents.TnTEvent, false, cu},
		{both, intents.TnTAlert, false, cu},
		// Nothing to go on: the whole node.
		{both, "", false, ""},
		{both, "ulp.unknown", false, ""},
		{both, intents.Error, false, ""},
		// A side the node doesn't have: the whole node.
		{shipper, intents.BookingRequest, false, ""},
		{shipper, intents.TnTEvent, true, ""},
		{carrier, intents.BookingRequest, true, ""},
		{carrier, intents.TnTEvent, false, ""},
		{carrier, intents.BookingRequest, false, op},
		{shipper, intents.TnTEvent, false, cu},
	} {
		if got := tc.a.sideOf(tc.opener, tc.openedHere); got != tc.want {
			t.Errorf("carrier %v, customer %v: sideOf(%q, here %v) = %q, want %q",
				tc.a.Carrier != nil, tc.a.Customer != nil, tc.opener, tc.openedHere, got, tc.want)
		}
	}
}

// TestMessageLogReplay: a message refused after its signature verified (a
// replay) is logged as verified, with its body, beside the signed
// ulp.error that answered it; both nodes put the exchange on its side.
func TestMessageLogReplay(t *testing.T) {
	p := startPair(t, "")
	h := intents.HeaderFor(intents.BookingRequest, "cymbal-shoes.example", "ocean-blue.example", time.Now())
	if _, err := p.shipper.Node.Exchange(context.Background(), h, request(), intents.BookingConfirmation); err != nil {
		t.Fatal(err)
	}
	if _, err := p.shipper.Node.Exchange(context.Background(), h, request(), intents.BookingConfirmation); err == nil {
		t.Fatal("the replay was accepted")
	}
	ob := p.operator.Messages.List(store.MessageFilter{Since: 2})
	if len(ob) != 2 {
		t.Fatalf("operator kept %d messages for the replay: %+v", len(ob), ob)
	}
	if m := ob[0]; m.Direction != "in" || m.Outcome != "rejected" || m.Code != intents.CodeReplay || !m.Verified || m.Body == nil ||
		m.Side != activity.SideOperator {
		t.Errorf("the replay: %+v", m)
	}
	if m := ob[1]; m.Direction != "out" || m.Intent != intents.Error || m.Side != activity.SideOperator || m.Conversation != h.CorrelationID {
		t.Errorf("the ulp.error: %+v", m)
	}
	cy := p.shipper.Messages.List(store.MessageFilter{Since: 2})
	if m := find(cy, "in", intents.Error); m == nil || !m.Verified || m.Side != activity.SideCustomer || m.Wire != ob[1].Wire {
		t.Errorf("the shipper's copy of the ulp.error: %+v", m)
	}
}

// TestWebUISides: on a node that is carrier and shipper, each persona
// sees only its roles' messages through the UI, the protocol endpoints
// share the listener, and a node without a web section serves no UI.
func TestWebUISides(t *testing.T) {
	p := startPairWith(t, "", `roles: [shipper, operator]
control: { socket: "`+sockDir(t)+`/cy.sock" }
operator: { refPrefix: CY-BKG, containerPrefix: CYMU, vessel: Sneaker, voyage: 1E, departureHour: 6, transit: 48h }
web:
  auth: demo
  personas:
    - { id: priya, name: Priya, title: Procurement, roles: [shipper] }
    - { id: sam, name: Sam, title: Ops desk, roles: [operator] }`)
	if _, err := p.shipper.Shipper.Book(context.Background(), "ocean-blue.example", request()); err != nil {
		t.Fatal(err)
	}
	h := intents.HeaderFor(intents.BookingRequest, "ocean-blue.example", "cymbal-shoes.example", time.Now())
	req := request()
	req.ShipperName = "Ocean Blue"
	if _, err := p.operator.Node.Exchange(context.Background(), h, req, intents.BookingConfirmation); err != nil {
		t.Fatal(err)
	}
	srv := p.shipper.Server.Handler
	call := func(method, path, body string, c *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Host = "cymbal-shoes.localhost"
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		if c != nil {
			r.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, r)
		return rec
	}
	signIn := func(id string) *http.Cookie {
		rec := call(http.MethodPost, "/ui/api/login", `{"persona":"`+id+`"}`, nil)
		if rec.Code != http.StatusOK || len(rec.Result().Cookies()) != 1 {
			t.Fatalf("login %s: HTTP %d", id, rec.Code)
		}
		return rec.Result().Cookies()[0]
	}
	for id, side := range map[string]string{"priya": activity.SideCustomer, "sam": activity.SideOperator} {
		var r MessagesResponse
		rec := call(http.MethodGet, "/ui/api/messages?side=customer&side=operator", "", signIn(id))
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &r) != nil || len(r.Messages) < 2 {
			t.Fatalf("%s: HTTP %d %s", id, rec.Code, rec.Body)
		}
		for _, m := range r.Messages {
			if m.Side != side && m.Side != "" {
				t.Errorf("%s saw a %s-side message: %s %s", id, m.Side, m.Direction, m.Intent)
			}
		}
	}
	if rec := call(http.MethodGet, "/ui/api/messages", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("messages without a session: HTTP %d", rec.Code)
	}
	if rec := call(http.MethodGet, "/.well-known/ulp", "", nil); rec.Code != http.StatusOK {
		t.Errorf("the profile beside the UI: HTTP %d", rec.Code)
	}
	if rec := call(http.MethodGet, "/ui/", "", nil); rec.Code != http.StatusOK {
		t.Errorf("the UI: HTTP %d", rec.Code)
	}
	r := httptest.NewRequest(http.MethodGet, "/ui/", nil)
	r.Host = "ocean-blue.localhost"
	rec := httptest.NewRecorder()
	p.operator.Server.Handler.ServeHTTP(rec, r)
	if rec.Code != http.StatusNotFound {
		t.Errorf("a node without a web section served /ui/: HTTP %d", rec.Code)
	}
}

// TestWebAssistantBooks: through the node's own listener, the browser's
// assistant drafts, and a person's confirmation books with the operator
// over the real protocol: the booking is the operator's and the shipper's.
func TestWebAssistantBooks(t *testing.T) {
	webui.ArmDelay = 0
	defer func() { webui.ArmDelay = 1500 * time.Millisecond }()
	p := startPairWith(t, "", `role: shipper
llm: { provider: fake }
web:
  auth: demo
  personas:
    - { id: alex, name: Alex, roles: [shipper] }`)
	srv := p.shipper.Server.Handler
	call := func(path, body string, c *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Host = "cymbal-shoes.localhost"
		r.Header.Set("Content-Type", "application/json")
		if c != nil {
			r.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, r)
		return rec
	}
	rec := call("/ui/api/login", `{"persona":"alex"}`, nil)
	c := rec.Result().Cookies()[0]
	var st webui.AssistantResponse
	rec = call("/ui/api/assistant/turn", `{"text":"Book one 40ft high-cube of sneakers for CY-PO-1, Rotterdam to Newark, departing 2026-10-06, with ocean-blue.example."}`, c)
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &st) != nil || len(st.Drafts) != 1 {
		t.Fatalf("turn: HTTP %d %s", rec.Code, rec.Body)
	}
	if p.operator.Carrier.Bookings.Len() != 0 {
		t.Fatal("a draft reached the operator")
	}
	// The assistant's model calls are in the node's activity, whose and
	// which tools, but nothing that was said.
	past, _, _, cancel := p.shipper.Activity.Subscribe(0, 0)
	cancel()
	var calls []activity.Model
	for _, e := range past {
		if e.Kind == activity.KindModel && e.Side == activity.SideCustomer {
			calls = append(calls, *e.Model)
		}
		if b, _ := json.Marshal(e); bytes.Contains(b, []byte("sneakers")) || bytes.Contains(b, []byte("CY-PO-1")) {
			t.Errorf("an activity entry carries the chat: %s", b)
		}
	}
	if len(calls) != 2 || calls[0].Agent != "assistant · alex" || len(calls[0].Tools) != 1 || calls[0].Tools[0] != assistant.ToolDraftBooking ||
		len(calls[1].Tools) != 0 || calls[0].Model != "fake" || calls[0].PromptHash == "" {
		t.Errorf("the assistant's model calls: %+v", calls)
	}
	rec = call("/ui/api/assistant/send", `{"draftId":"`+st.Drafts[0].DraftID+`"}`, c)
	if rec.Code != http.StatusOK {
		t.Fatalf("send: HTTP %d %s", rec.Code, rec.Body)
	}
	if p.operator.Carrier.Bookings.Len() != 1 || p.shipper.Customer.Bookings.Len() != 1 {
		t.Errorf("bookings: operator %d, shipper %d", p.operator.Carrier.Bookings.Len(), p.shipper.Customer.Bookings.Len())
	}
}
