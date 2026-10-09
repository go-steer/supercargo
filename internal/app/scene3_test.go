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
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/activity"
	"github.com/go-steer/supercargo/internal/config"
	"github.com/go-steer/supercargo/internal/control"
	"github.com/go-steer/supercargo/internal/operator"
	"github.com/go-steer/supercargo/internal/shipper"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/intents"
)

// syncBuffer is a log sink two goroutines can share.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestScene3: two nodes assembled from config, over real sockets. A
// booking starts a voyage; its events reach the shipper, verified and
// recorded; a disruption through the control API reaches it as an alert.
func TestScene3(t *testing.T) {
	opAddr, shAddr := freePort(t), freePort(t)
	sockets := t.TempDir()
	if err := os.Chmod(sockets, 0o700); err != nil {
		t.Fatal(err)
	}
	opSock, shSock := sockets+"/ob.sock", sockets+"/cy.sock"
	peers := "peers: { ocean-blue.example: \"" + opAddr + "\", cymbal-shoes.example: \"" + shAddr + "\" }\n"
	opCfg, err := config.Parse([]byte(`domain: ocean-blue.example
name: Ocean Blue
role: operator
listen: "` + opAddr + `"
tls: { mode: insecure-dev }
keys: [{ kid: ob-1, privateKeyFile: "` + keyFile(t) + `" }]
` + peers + `operator: { refPrefix: OB-BKG, containerPrefix: OCBU, vessel: Ocean Blue Aurora, voyage: 42W, departureHour: 18, transit: 400h } # arrival 23s in: room to disrupt
simulator: { hoursPerSecond: 20 }
llm: { provider: fake }
control: { socket: "` + opSock + `" }
`))
	if err != nil {
		t.Fatal(err)
	}
	shCfg, err := config.Parse([]byte(`domain: cymbal-shoes.example
name: Cymbal Shoes
role: shipper
listen: "` + shAddr + `"
tls: { mode: insecure-dev }
keys: [{ kid: cy-1, privateKeyFile: "` + keyFile(t) + `" }]
control: { socket: "` + shSock + `" }
llm: { provider: fake }
` + peers))
	if err != nil {
		t.Fatal(err)
	}
	var opLog, shLog syncBuffer
	op, err := New(opCfg, slog.New(slog.NewJSONHandler(&opLog, nil)))
	if err != nil {
		t.Fatal(err)
	}
	sh, err := New(shCfg, slog.New(slog.NewJSONHandler(&shLog, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 2)
	go func() { done <- op.Serve(ctx) }()
	go func() { done <- sh.Serve(ctx) }()
	defer func() {
		cancel()
		for range 2 {
			if err := <-done; err != nil {
				t.Errorf("Serve: %v", err)
			}
		}
	}()
	waitListening(t, opAddr)
	waitListening(t, shAddr)
	waitUnix(t, opSock)
	waitUnix(t, shSock)

	// The shipper books through its node's control socket, as ulpnode book
	// does, so the running node records the booking.
	var br shipper.BookResponse
	code, err := control.Call(control.Client(shSock, 10*time.Second), "/book", shipper.BookRequest{
		Operator: "ocean-blue.example",
		Request: intents.BookingRequestBody{
			ShipperRef: "CY-PO-55120", EarliestDeparture: "2026-10-06",
			ShipperName:     "Cymbal Shoes",
			PortOfLoading:   "NLRTM",
			PortOfDischarge: "USEWR",
			Equipment:       []intents.Equipment{{ISOType: "45G1", Count: 1, GrossWeightKg: 12000}},
			Cargo:           intents.Cargo{Description: "Footwear (sneakers)", HSCode: "6404"},
			Subscription: &intents.Subscription{Endpoint: "http://cymbal-shoes.example/ulp",
				Intents: []string{intents.TnTEvent, intents.TnTAlert}},
		},
	}, &br)
	if err != nil || code != http.StatusOK || br.Confirmation == nil || br.Confirmation.Status != intents.StatusConfirmed {
		t.Fatalf("book: HTTP %d %+v, %v", code, br, err)
	}
	ref := br.Confirmation.BookingRef

	// Scene 3: gate-in, load, departure, and the arrival estimate arrive
	// (27h of lead-in at 20h/s, after a 2s start delay).
	codes := func() []string {
		var out []string
		for _, r := range sh.Customer.Events.List("ocean-blue.example", ref) {
			var e intents.TnTEventBody
			_ = json.Unmarshal(r.Body, &e)
			if r.Intent == intents.TnTEvent {
				out = append(out, e.EventTypeCode+" "+e.EventClassifierCode)
			} else {
				out = append(out, "alert")
			}
		}
		return out
	}
	waitUntil(t, 10*time.Second, func() bool { return len(codes()) >= 4 })
	if got := strings.Join(codes()[:4], ", "); got != "GTIN ACT, LOAD ACT, DEPA ACT, ARRI EST" {
		t.Errorf("shipper recorded %s", got)
	}
	for _, r := range sh.Customer.Events.List("ocean-blue.example", ref) {
		if r.Header.Sender != "ocean-blue.example" || r.Header.BookingRef == nil || *r.Header.BookingRef != ref {
			t.Errorf("record header %+v", r.Header)
		}
	}

	// Scene 4's trigger: the presenter disrupts the voyage.
	var dr operator.DisruptResponse
	code, err = control.Call(control.Client(opSock, 10*time.Second), "/disrupt",
		operator.DisruptRequest{BookingRef: ref, Delay: "72h", Reason: "Engine repair, unscheduled call at Halifax."}, &dr)
	if err != nil || code != http.StatusOK || dr.Alert == nil || dr.Alert.Severity != intents.SeverityHigh {
		t.Fatalf("disrupt: HTTP %d %+v, %v", code, dr, err)
	}
	var alert *store.Record
	waitUntil(t, 10*time.Second, func() bool {
		for _, r := range sh.Customer.Events.List("ocean-blue.example", ref) {
			if r.Intent == intents.TnTAlert {
				alert = &r
				return true
			}
		}
		return false
	})
	var got intents.TnTAlertBody
	if err := json.Unmarshal(alert.Body, &got); err != nil || got.AlertID != dr.Alert.AlertID || got.NewETA != dr.Alert.NewETA {
		t.Errorf("shipper's alert %+v (%v), operator's %+v", got, err, dr.Alert)
	}

	// Scene 5: the shipper asks why, through its node; the operator's
	// (fake) model answers from this booking's records, citing them.
	var ar shipper.AskResponse
	code, err = control.Call(control.Client(shSock, 10*time.Second), "/ask",
		shipper.AskRequest{BookingRef: ref, Question: "What caused the delay, and how firm is the new ETA?"}, &ar)
	if err != nil || code != http.StatusOK || ar.Answer == nil {
		t.Fatalf("ask: HTTP %d %+v, %v", code, ar, err)
	}
	if ar.Answer.GeneratedBy != intents.GeneratedByLLM || !strings.Contains(ar.Answer.Answer, "Halifax") ||
		len(ar.Held) == 0 || len(ar.Held) != len(ar.Answer.CitedEventIDs) {
		t.Errorf("answer %+v, held %v", ar.Answer, ar.Held)
	}
	if !strings.Contains(opLog.String(), `"msg":"model call"`) || !strings.Contains(opLog.String(), `"outcome":"ok"`) {
		t.Error("the operator didn't log the model call")
	}
	if !strings.Contains(opLog.String(), `"intent":"ulp.tnt.inquiry","sender":"cymbal-shoes.example","receiver":"ocean-blue.example","via":"a2a"`) {
		t.Error("the inquiry didn't arrive over A2A")
	}

	// The control API shows the same story: the shipper's shipments, and
	// both nodes' activity streams, replayed from the start.
	var shipments shipper.ShipmentsResponse
	resp, err := control.Client(shSock, 5*time.Second).Get("http://control/shipments")
	if err != nil {
		t.Fatal(err)
	}
	_ = json.NewDecoder(resp.Body).Decode(&shipments)
	resp.Body.Close()
	if len(shipments.Shipments) != 1 || shipments.Shipments[0].Ref != ref || shipments.Shipments[0].Alerts != 1 ||
		shipments.Shipments[0].ETA != dr.Alert.NewETA {
		t.Errorf("shipments %+v", shipments)
	}
	// missing reads sock's stream from the start until every wanted entry
	// has been seen (or 5s pass), and returns those that weren't.
	missing := func(sock string, want ...string) []string {
		left := map[string]bool{}
		for _, w := range want {
			left[w] = true
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = activity.Stream(ctx, control.Client(sock, 0), "http://control/events", 0, func(e activity.Entry, _ error) error {
			key := e.Kind
			switch {
			case e.Envelope != nil:
				key += " " + e.Envelope.Direction + " " + e.Envelope.Intent + " via " + e.Envelope.Via
			case e.Record != nil:
				key += " " + e.Record.Action
			case e.Model != nil:
				key += " " + e.Model.Agent
			}
			delete(left, key)
			if len(left) == 0 {
				return io.EOF // done
			}
			return nil
		})
		var out []string
		for k := range left {
			out = append(out, k)
		}
		return out
	}
	if m := missing(shSock, "envelope in ulp.tnt.event via rest", "record recorded", "envelope out ulp.tnt.inquiry via a2a",
		"summary", "model alert summarizer"); len(m) != 0 {
		t.Errorf("the shipper's stream lacks %v", m)
	}
	if m := missing(opSock, "voyage", "record published", "model inquiry responder", "envelope in ulp.tnt.inquiry via a2a"); len(m) != 0 {
		t.Errorf("the operator's stream lacks %v", m)
	}

	// The logs tell the story: the operator published, the shipper
	// verified and accepted, and showed the alert.
	for _, want := range []string{`"msg":"voyage started"`, `"msg":"event published"`, `"msg":"alert published"`} {
		if !strings.Contains(opLog.String(), want) {
			t.Errorf("operator log lacks %s", want)
		}
	}
	for _, want := range []string{`"intent":"ulp.tnt.event"`, `"verified":true`, `"msg":"event recorded"`, `"msg":"alert recorded"`, `"severity":"high"`,
		`"msg":"alert summary"`, `"generatedBy":"llm"`} {
		if !strings.Contains(shLog.String(), want) {
			t.Errorf("shipper log lacks %s", want)
		}
	}
	if strings.Contains(opLog.String(), "push dropped") || strings.Contains(shLog.String(), `"outcome":"rejected"`) {
		t.Errorf("a push was dropped or rejected:\n%s\n%s", opLog.String(), shLog.String())
	}
}

func waitUnix(t *testing.T, path string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if c, err := net.Dial("unix", path); err == nil {
			c.Close()
			return
		}
	}
	t.Fatalf("nothing listening on %s", path)
}

func waitUntil(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("condition not reached")
}
