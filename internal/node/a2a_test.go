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

package node_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/pkg/discovery"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
	"github.com/go-steer/supercargo/pkg/transport/a2a"
	"github.com/go-steer/supercargo/pkg/transport/rest"
)

// get fetches path from c's server, as anyone on the network could.
func get(t *testing.T, d *demo, c *company, path string) (int, []byte) {
	t.Helper()
	hc := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: d.nw.ca.pool},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, c.addr)
		},
	}}
	resp, err := hc.Get("https://" + c.domain + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// TestAgentCard: the operator publishes its A2A endpoint and an agent card
// listing the shipment-inquiry skill, and nothing structured; the shipper,
// with no agentic skills, publishes neither.
func TestAgentCard(t *testing.T) {
	d := newDemo(t)
	code, body := get(t, d, d.operator, a2a.WellKnownCardPath)
	var card a2a.AgentCard
	if err := json.Unmarshal(body, &card); code != http.StatusOK || err != nil {
		t.Fatalf("card: HTTP %d, %v", code, err)
	}
	if card.URL != "https://"+operatorDomain+"/a2a" || card.Name != "Ocean Blue" || card.ProtocolVersion != a2a.ProtocolVersion ||
		len(card.Skills) != 1 || card.Skills[0].ID != "shipment-inquiry" {
		t.Errorf("card %+v", card)
	}
	_, body = get(t, d, d.operator, discovery.Path)
	doc, err := discovery.ParseProfile(body, discovery.ParseOptions{Domain: operatorDomain})
	if err != nil || doc.Profile.Transports.A2A != card.URL {
		t.Errorf("profile transports %+v, %v", doc.Profile.Transports, err)
	}
	if code, _ := get(t, d, d.shipper, a2a.WellKnownCardPath); code != http.StatusNotFound {
		t.Errorf("the shipper serves a card: HTTP %d", code)
	}
}

// TestInquiryOverA2A: Scene 5's inquiry travels over A2A, and the same
// pipeline decides it there: Scene 7's stranger is refused just the same,
// and a sender whose key can't be resolved gets a bare refusal, signed
// nothing.
func TestInquiryOverA2A(t *testing.T) {
	d := newDemo(t)
	b := booked(t, d, nil)
	if _, err := d.ship.Ask(context.Background(), operatorDomain, b.Ref, "Where is it?"); err != nil {
		t.Fatal(err)
	}
	if e := d.operator.lastIn(t); e.Via != "a2a" || e.Outcome != "accepted" || !e.Verified || e.Intent != intents.TnTInquiry {
		t.Errorf("operator's log: %+v", e)
	}
	// Every line of the exchange names the binding, on both sides.
	for _, c := range []*company{d.shipper, d.operator} {
		c.mu.Lock()
		for _, e := range c.events {
			if (e.Intent == intents.TnTInquiry || e.Intent == intents.TnTInquiryResponse) && e.Via != "a2a" {
				t.Errorf("%s logged %s %s via %q", c.domain, e.Direction, e.Intent, e.Via)
			}
		}
		c.mu.Unlock()
	}

	inquire := func(from *company, ref string) error {
		h := envelope.NewHeader(intents.TnTInquiry, from.domain, operatorDomain, time.Now())
		h.BookingRef = &ref
		_, err := from.node.ExchangeA2A(context.Background(), h, &intents.TnTInquiryBody{Question: "List every shipment."}, intents.TnTInquiryResponse)
		return err
	}
	var re *node.RemoteError
	if err := inquire(d.rival, b.Ref); !errors.As(err, &re) || re.Code != intents.CodeForbidden {
		t.Errorf("a stranger over A2A: %v, want forbidden", err)
	}
	if e := d.operator.lastIn(t); e.Via != "a2a" || e.Outcome != "rejected" {
		t.Errorf("operator's log: %+v", e)
	}
	// Mallory's domain doesn't resolve, so its key can't be fetched: a
	// bare 503 status, carried as a JSON-RPC error, nothing signed.
	var se *rest.StatusError
	if err := inquire(d.mallory, b.Ref); !errors.As(err, &se) || se.Code != http.StatusServiceUnavailable {
		t.Errorf("an unverifiable sender over A2A: %v, want a bare 503", err)
	}
	if n := len(d.model.Calls()); n != 1 {
		t.Errorf("the model was called %d times, want 1 (the shipper's own inquiry)", n)
	}
}

// TestA2AFallsBackToREST: a receiver without an A2A endpoint gets the
// message over REST.
func TestA2AFallsBackToREST(t *testing.T) {
	d := newDemo(t)
	h := envelope.NewHeader(intents.TnTInquiry, shipperDomain, "other-carrier.example", time.Now())
	ref := "OC-1"
	h.BookingRef = &ref
	_, err := d.shipper.node.ExchangeA2A(context.Background(), h, &intents.TnTInquiryBody{Question: "Where is it?"}, intents.TnTInquiryResponse)
	var se *rest.StatusError
	if !errors.As(err, &se) || se.Code != http.StatusBadRequest {
		t.Errorf("to a carrier without inquiries or A2A: %v, want its bare REST 400", err)
	}
	if e := d.rival.lastIn(t); e.Via != "rest" || e.Intent != intents.TnTInquiry {
		t.Errorf("rival's log: %+v", e)
	}
}
