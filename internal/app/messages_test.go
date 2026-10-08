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
	"github.com/go-steer/supercargo/internal/store"
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
