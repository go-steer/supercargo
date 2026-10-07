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

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/go-steer/supercargo/internal/config"
	"github.com/go-steer/supercargo/internal/control"
	"github.com/go-steer/supercargo/internal/shipper"
	"github.com/go-steer/supercargo/pkg/intents"
	"github.com/go-steer/supercargo/pkg/sig"
)

func runCmd(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err := run(args, strings.NewReader(stdin), &out, &errOut)
	return out.String() + errOut.String(), err
}

func TestUsage(t *testing.T) {
	if _, err := runCmd(t, ""); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("no command: %v", err)
	}
	if _, err := runCmd(t, "", "launch"); err == nil {
		t.Error("unknown command accepted")
	}
}

func TestKeygen(t *testing.T) {
	p := filepath.Join(t.TempDir(), "key.pem")
	out, err := runCmd(t, "", "keygen", "--kid", "cy-2026-1", "--out", p)
	if err != nil {
		t.Fatal(err)
	}
	kid, v, err := sig.ParseJWK([]byte(strings.TrimSpace(out)))
	if err != nil || kid != "cy-2026-1" || v.Alg() != sig.Ed25519 {
		t.Errorf("keygen printed %q: %v", out, err)
	}
	if info, _ := os.Stat(p); info.Mode().Perm() != 0o600 {
		t.Errorf("key mode %v", info.Mode().Perm())
	}
	if _, err := runCmd(t, "", "keygen", "--kid", "cy-2026-1", "--out", p); err == nil {
		t.Error("keygen overwrote an existing key")
	}
	for _, args := range [][]string{
		{"keygen", "--out", filepath.Join(t.TempDir(), "k.pem")},
		{"keygen", "--kid", "bad kid", "--out", filepath.Join(t.TempDir(), "k.pem")},
		{"keygen", "--kid", "k1"},
		{"keygen", "--nope"},
	} {
		if _, err := runCmd(t, "", args...); err == nil {
			t.Errorf("%v succeeded", args)
		}
	}
}

func shipperConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "key.pem")
	if _, err := runCmd(t, "", "keygen", "--kid", "cy-1", "--out", key); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "shipper.yaml")
	y := "domain: cymbal-shoes.example\nname: Cymbal\nrole: shipper\nlisten: \"127.0.0.1:0\"\ntls: { mode: insecure-dev }\n" +
		"keys: [{ kid: cy-1, privateKeyFile: \"" + key + "\" }]\n" +
		"peers: { ocean-blue.example: \"127.0.0.1:1\" }\n" + // offline: a refused connection, not a DNS lookup
		"control: { socket: \"" + filepath.Join(dir, "cy.sock") + "\" }\n"
	if err := os.WriteFile(p, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const requestJSON = `{"shipper_reference":"P1","shipper_name":"Cymbal Shoes","port_of_loading":"NLRTM","port_of_discharge":"USEWR",
"containers":[{"equipment_size_type":"45G1","count":1,"gross_weight_kg":12000}],"cargo":{"description":"Shoes","hs_code":"6404"},"earliest_departure":"2026-10-06"}`

func writeFile(t *testing.T, name, data string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// fakeNode stands in for the running shipper node behind a config's
// control socket: it answers POST /book with a fixed status (a
// confirmation if 200) and records what it was sent.
type fakeNode struct {
	mu    sync.Mutex
	sent  []shipper.BookRequest
	asked []shipper.AskRequest
}

func (f *fakeNode) calls() []shipper.BookRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]shipper.BookRequest(nil), f.sent...)
}

func startNode(t *testing.T, cfg string, status int) *fakeNode {
	t.Helper()
	c, err := config.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	l, err := control.Listen(c.Control.Socket)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeNode{}
	srv := control.Server(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/status" {
			var req shipper.StatusRequest
			if err := control.Decode(r, &req); err != nil {
				t.Errorf("decode: %v", err)
			}
			if status != http.StatusOK {
				control.Answer(w, status, shipper.StatusResponse{Error: "no booking " + req.BookingRef})
				return
			}
			control.Answer(w, status, shipper.StatusResponse{Operator: "ocean-blue.example", Missing: []string{"EV-2"},
				Status: &intents.TnTStatus{TransportDocumentReference: "OB-TD-1042", DCSAStatus: intents.StatusInTransit,
					VesselName: "Ocean Blue Aurora", VoyageNumber: "42W", PortOfLoading: "NLRTM", PortOfDischarge: "USEWR",
					CurrentLocation:        &intents.CurrentLocation{UNLocationCode: "NLRTM", FacilityName: "Port of Rotterdam"},
					EstimatedTimeOfArrival: "2026-10-16T08:00:00Z", Events: []intents.TnTEventBody{
						{EventID: "EV-1", EventType: "TRANSPORT", EventClassifierCode: "ACT", EventTypeCode: "DEPA", EventDateTime: "2026-10-07T18:00:00Z", Description: "Departed"},
						{EventID: "EV-2", EventType: "TRANSPORT", EventClassifierCode: "EST", EventTypeCode: "ARRI", EventDateTime: "2026-10-16T08:00:00Z", Description: "Expected"},
					}}})
			return
		}
		if r.URL.Path == "/ask" {
			var req shipper.AskRequest
			if err := control.Decode(r, &req); err != nil {
				t.Errorf("decode: %v", err)
			}
			f.mu.Lock()
			f.asked = append(f.asked, req)
			f.mu.Unlock()
			if status != http.StatusOK {
				control.Answer(w, status, shipper.AskResponse{Error: "no booking " + req.BookingRef})
				return
			}
			control.Answer(w, status, shipper.AskResponse{Operator: "ocean-blue.example", Held: []string{"EV-1"},
				Answer: &intents.TnTInquiryResponseBody{Answer: "Departed Rotterdam.\nArriving the 16th.", CitedEventIDs: []string{"EV-1", "AL-2"},
					GeneratedBy: intents.GeneratedByLLM, DataScope: intents.DataScope{BookingRef: req.BookingRef, EventsConsidered: 3}}})
			return
		}
		var req shipper.BookRequest
		if err := control.Decode(r, &req); err != nil {
			t.Errorf("decode: %v", err)
		}
		f.mu.Lock()
		f.sent = append(f.sent, req)
		f.mu.Unlock()
		if status != http.StatusOK {
			control.Answer(w, status, shipper.BookResponse{Error: "node: partner refused"})
			return
		}
		control.Answer(w, status, shipper.BookResponse{Confirmation: &intents.BookingConfirmationBody{BookingRef: "OB-BKG-1042", Status: intents.StatusConfirmed}})
	}), nil)
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return f
}

// TestBookNeedsAYes: anything but "y" sends nothing.
func TestBookNeedsAYes(t *testing.T) {
	cfg := shipperConfig(t)
	node := startNode(t, cfg, http.StatusOK)
	req := writeFile(t, "req.json", requestJSON)
	for _, answer := range []string{"n\n", "\n", "yes please\n", ""} {
		out, err := runCmd(t, answer, "book", "--config", cfg, "--operator", "ocean-blue.example", "--request", req)
		if !errors.Is(err, errNotSent) || !strings.Contains(out, "Not sent.") {
			t.Errorf("answer %q: %q, %v; want errNotSent", answer, out, err)
		}
	}
	if n := len(node.calls()); n != 0 {
		t.Errorf("the node was asked to book %d times", n)
	}
}

// TestBookSends: a "y" (or --yes) sends the booking through the running
// node, and prints the confirmation.
func TestBookSends(t *testing.T) {
	cfg := shipperConfig(t)
	node := startNode(t, cfg, http.StatusOK)
	req := writeFile(t, "req.json", requestJSON)
	out, err := runCmd(t, "y\n", "book", "--config", cfg, "--operator", "ocean-blue.example", "--request", req)
	if err != nil || !strings.Contains(out, `"carrier_booking_reference": "OB-BKG-1042"`) {
		t.Fatalf("book: %q, %v", out, err)
	}
	if _, err := runCmd(t, "", "book", "--config", cfg, "--operator", "ocean-blue.example", "--request", req, "--yes"); err != nil {
		t.Fatal(err)
	}
	calls := node.calls()
	if len(calls) != 2 || calls[0].Operator != "ocean-blue.example" || calls[0].Request.ShipperRef != "P1" {
		t.Errorf("node was sent %+v", calls)
	}
}

// TestBookSummary: the person sees where updates go and who the consignee
// is before saying yes.
func TestBookSummary(t *testing.T) {
	cfg := shipperConfig(t)
	startNode(t, cfg, http.StatusOK)
	withAll := strings.Replace(requestJSON, `"earliest_departure":"2026-10-06"`,
		`"earliest_departure":"2026-10-06","consignee":{"name":"Cymbal Footwear","domain":"cymbal-footwear.example"},`+
			`"subscription":{"endpoint":"https://cymbal-shoes.example/ulp","intents":["ulp.tnt.event"]}`, 1)
	out, _ := runCmd(t, "n\n", "book", "--config", cfg, "--operator", "ocean-blue.example", "--request", writeFile(t, "r.json", withAll))
	for _, want := range []string{"cymbal-footwear.example", "https://cymbal-shoes.example/ulp", "ulp.tnt.event", "1 × 40HC", "Shoes", "NLRTM", "Rotterdam", "12000 kg", "Cymbal Shoes"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary lacks %q:\n%s", want, out)
		}
	}
	out, _ = runCmd(t, "n\n", "book", "--config", cfg, "--operator", "ocean-blue.example", "--request", writeFile(t, "r.json", requestJSON))
	if !strings.Contains(out, "none requested") {
		t.Errorf("summary without a subscription:\n%s", out)
	}
}

// TestKeygenBadKidLeavesNoFile: a refused kid writes no key.
func TestKeygenBadKidLeavesNoFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "k.pem")
	if _, err := runCmd(t, "", "keygen", "--kid", "bad kid", "--out", p); err == nil {
		t.Fatal("bad kid accepted")
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Error("a key file was left behind")
	}
}

func TestBookRejects(t *testing.T) {
	cfg := shipperConfig(t)
	opCfg := writeFile(t, "op.yaml", strings.Replace(mustRead(t, cfg), "role: shipper",
		"role: operator\noperator: { refPrefix: OB, containerPrefix: OCBU, vessel: V, voyage: 1W, departureHour: 1, transit: 1h }", 1))
	good := writeFile(t, "req.json", requestJSON)
	noControl := writeFile(t, "nc.yaml", strings.Split(mustRead(t, cfg), "control:")[0])
	for name, args := range map[string][]string{
		"no flags":          {"book"},
		"missing config":    {"book", "--config", filepath.Join(t.TempDir(), "none.yaml"), "--operator", "o.example", "--request", good},
		"operator config":   {"book", "--config", opCfg, "--operator", "o.example", "--request", good},
		"missing request":   {"book", "--config", cfg, "--operator", "o.example", "--request", filepath.Join(t.TempDir(), "none.json")},
		"unknown field":     {"book", "--config", cfg, "--operator", "o.example", "--request", writeFile(t, "r.json", strings.Replace(requestJSON, `"shipper_reference"`, `"deadline":"x","shipper_reference"`, 1))},
		"invalid body":      {"book", "--config", cfg, "--operator", "o.example", "--request", writeFile(t, "r.json", strings.Replace(requestJSON, "NLRTM", "nowhere", 1))},
		"not JSON":          {"book", "--config", cfg, "--operator", "o.example", "--request", writeFile(t, "r.json", "{")},
		"bad flag":          {"book", "--nope"},
		"node not running":  {"book", "--config", cfg, "--operator", "ocean-blue.example", "--request", good, "--yes"},
		"no control socket": {"book", "--config", noControl, "--operator", "ocean-blue.example", "--request", good, "--yes"},
	} {
		if _, err := runCmd(t, "", args...); err == nil {
			t.Errorf("%s: succeeded", name)
		}
	}
	// The node couldn't book: an error, not a confirmation.
	startNode(t, cfg, http.StatusBadGateway)
	if out, err := runCmd(t, "", "book", "--config", cfg, "--operator", "ocean-blue.example", "--request", good, "--yes"); err == nil ||
		!strings.Contains(err.Error(), "partner refused") {
		t.Errorf("a failed booking: %q, %v", out, err)
	}
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestServeRejects(t *testing.T) {
	cfg := shipperConfig(t)
	// A config that loads but whose key is gone: the node can't start
	// (after telemetry is set up, and torn down again).
	keyless := shipperConfig(t)
	_ = os.Remove(filepath.Join(filepath.Dir(keyless), "key.pem"))
	for name, args := range map[string][]string{
		"key gone":       {"serve", "--config", keyless},
		"no config":      {"serve"},
		"bad log format": {"serve", "--config", cfg, "--log-format", "xml"},
		"missing config": {"serve", "--config", filepath.Join(t.TempDir(), "none.yaml")},
		"bad flag":       {"serve", "--nope"},
	} {
		if _, err := runCmd(t, "", args...); err == nil {
			t.Errorf("%s: succeeded", name)
		}
	}
}

func TestLogger(t *testing.T) {
	for _, f := range []string{"text", "json", "pretty"} {
		if _, err := logger(f, "dark", &bytes.Buffer{}); err != nil {
			t.Errorf("logger(%s): %v", f, err)
		}
	}
	if _, err := logger("pretty", "neon", &bytes.Buffer{}); err == nil {
		t.Error("--log-theme neon accepted")
	}
	if _, err := logger("yaml", "dark", &bytes.Buffer{}); err == nil {
		t.Error("--log-format yaml accepted")
	}
	// Pretty is coloured only on a terminal (TTY_FORCE stands in for one
	// here), never on a dumb one or with NO_COLOR, and downsampled to what
	// the terminal has.
	for name, c := range map[string]struct {
		env  []string
		want bool
	}{
		"a file or pipe": {[]string{"TERM=xterm-256color"}, false},
		"a terminal":     {[]string{"TTY_FORCE=1", "TERM=xterm-256color"}, true},
		"dumb":           {[]string{"TTY_FORCE=1", "TERM=dumb"}, false},
		"NO_COLOR":       {[]string{"TTY_FORCE=1", "TERM=xterm-256color", "NO_COLOR=1"}, false},
	} {
		if _, got := colorWriter(&bytes.Buffer{}, c.env); got != c.want {
			t.Errorf("%s: colour %v, want %v", name, got, c.want)
		}
	}
	var buf bytes.Buffer
	w, _ := colorWriter(&buf, []string{"TTY_FORCE=1", "TERM=xterm-256color"})
	_, _ = io.WriteString(w, "\x1b[38;2;33;123;254mx\x1b[m")
	if strings.Contains(buf.String(), "38;2;") || !strings.Contains(buf.String(), "38;5;") {
		t.Errorf("not downsampled to 256 colours: %q", buf.String())
	}
}

// TestAsk: the question goes through the running node; the answer prints
// with its sources, marked held or not.
func TestAsk(t *testing.T) {
	cfg := shipperConfig(t)
	node := startNode(t, cfg, http.StatusOK)
	out, err := runCmd(t, "", "ask", "--config", cfg, "--booking", "OB-BKG-1042", "Why", "the", "delay?")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ocean-blue.example answered (llm, from 3 records of OB-BKG-1042)", "  Departed Rotterdam.\n  Arriving the 16th.",
		"Sources: EV-1 (held, signed), AL-2 (not held)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	node.mu.Lock()
	asked := node.asked
	node.mu.Unlock()
	if len(asked) != 1 || asked[0].BookingRef != "OB-BKG-1042" || asked[0].Question != "Why the delay?" || asked[0].Operator != "" {
		t.Errorf("node was asked %+v", asked)
	}
}

func TestAskRejects(t *testing.T) {
	cfg := shipperConfig(t)
	opCfg := writeFile(t, "op.yaml", strings.Replace(mustRead(t, cfg), "role: shipper",
		"role: operator\noperator: { refPrefix: OB, containerPrefix: OCBU, vessel: V, voyage: 1W, departureHour: 1, transit: 1h }", 1))
	for name, args := range map[string][]string{
		"no question":      {"ask", "--config", cfg, "--booking", "B"},
		"no booking":       {"ask", "--config", cfg, "Where?"},
		"no config":        {"ask", "--booking", "B", "Where?"},
		"missing config":   {"ask", "--config", "/nonexistent.yaml", "--booking", "B", "Where?"},
		"operator config":  {"ask", "--config", opCfg, "--booking", "B", "Where?"},
		"bad flag":         {"ask", "--nope"},
		"node not running": {"ask", "--config", cfg, "--booking", "B", "Where?"},
	} {
		if _, err := runCmd(t, "", args...); err == nil {
			t.Errorf("%s: succeeded", name)
		}
	}
	startNode(t, cfg, http.StatusNotFound)
	if _, err := runCmd(t, "", "ask", "--config", cfg, "--booking", "B", "Where?"); err == nil || !strings.Contains(err.Error(), "no booking B") {
		t.Errorf("unknown booking: %v", err)
	}
}

func TestStartTelemetry(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	stop, err := startTelemetry(&config.Config{Domain: "cymbal-shoes.example"}, log)
	if err != nil || strings.Contains(buf.String(), "telemetry") {
		t.Fatalf("off: %v %s", err, buf.String())
	}
	stop()
	// A collector that's down: setup succeeds, the flush at exit warns.
	t.Cleanup(func() {
		otel.SetTracerProvider(tracenoop.NewTracerProvider())
		otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) {}))
	})
	stop, err = startTelemetry(&config.Config{Domain: "cymbal-shoes.example",
		Telemetry: &config.Telemetry{Protocol: "http", Endpoint: "127.0.0.1:1", Insecure: true}}, log)
	if err != nil || !strings.Contains(buf.String(), "exporting traces") {
		t.Fatalf("on: %v %s", err, buf.String())
	}
	_, span := otel.Tracer("test").Start(context.Background(), "x")
	span.End()
	stop()
	if !strings.Contains(buf.String(), "level=WARN msg=telemetry") {
		t.Errorf("no warning for a dead collector: %s", buf.String())
	}
}

// TestMain keeps a developer's own OTEL_* settings out of the tests: no
// test exports anywhere.
func TestMain(m *testing.M) {
	for _, k := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"} {
		_ = os.Unsetenv(k)
	}
	os.Exit(m.Run())
}

func TestStatusCommand(t *testing.T) {
	cfg := shipperConfig(t)
	startNode(t, cfg, http.StatusOK)
	out, err := runCmd(t, "", "status", "--config", cfg, "--booking", "OB-BKG-1042")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ocean-blue.example reports OB-BKG-1042 (transport document OB-TD-1042): IN_TRANSIT", "Vessel:   Ocean Blue Aurora 42W", "Route:    NLRTM → USEWR",
		"Now at:   NLRTM Port of Rotterdam", "ETA:      2026-10-16T08:00:00Z", "Departed\n", "Expected  (not received as a push)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestStatusCommandRejects(t *testing.T) {
	cfg := shipperConfig(t)
	opCfg := writeFile(t, "op.yaml", strings.Replace(mustRead(t, cfg), "role: shipper",
		"role: operator\noperator: { refPrefix: OB, containerPrefix: OCBU, vessel: V, voyage: 1W, departureHour: 1, transit: 1h }", 1))
	for name, args := range map[string][]string{
		"no booking":       {"status", "--config", cfg},
		"no config":        {"status", "--booking", "B"},
		"missing config":   {"status", "--config", "/nonexistent.yaml", "--booking", "B"},
		"operator config":  {"status", "--config", opCfg, "--booking", "B"},
		"bad flag":         {"status", "--nope"},
		"node not running": {"status", "--config", cfg, "--booking", "B"},
	} {
		if _, err := runCmd(t, "", args...); err == nil {
			t.Errorf("%s: succeeded", name)
		}
	}
	startNode(t, cfg, http.StatusNotFound)
	if _, err := runCmd(t, "", "status", "--config", cfg, "--booking", "B"); err == nil || !strings.Contains(err.Error(), "no booking B") {
		t.Errorf("a refusal: %v", err)
	}
}
