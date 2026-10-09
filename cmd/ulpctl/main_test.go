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
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/supercargo/internal/control"
	"github.com/go-steer/supercargo/internal/operator"
	"github.com/go-steer/supercargo/pkg/intents"
)

const operatorYAML = `domain: ocean-blue.example
name: Ocean Blue
role: operator
listen: ":8443"
tls: { mode: insecure-dev }
keys: [{ kid: ob-1, privateKeyFile: /nonexistent }]
operator: { refPrefix: OB-BKG, containerPrefix: OCBU, vessel: V, voyage: 1W, departureHour: 18, transit: 1h }
control: { socket: SOCKET }
`

// fakeNode serves /disrupt on a control socket, answering with a fixed
// alert, or with status and errText. It returns the operator config's path
// and what the node was sent.
func fakeNode(t *testing.T, status int, errText string) (cfg string, got *operator.DisruptRequest) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "ob.sock")
	l, err := control.Listen(socket)
	if err != nil {
		t.Fatal(err)
	}
	got = &operator.DisruptRequest{}
	srv := control.Server(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := control.Decode(r, got); err != nil {
			t.Errorf("decode: %v", err)
		}
		resp := operator.DisruptResponse{Error: errText}
		if status == http.StatusOK && got.Voyage != "" {
			// A voyage: one booking alerted, and a second refused with
			// errText, if any.
			alert := &intents.TnTAlertBody{AlertID: "AL-000010", Kind: "delay", Severity: "medium", Summary: got.Reason,
				PreviousETA: "2026-10-13T08:00:00Z", NewETA: "2026-10-14T08:00:00Z", CauseEventIDs: []string{"EV-000009"}}
			resp = operator.DisruptResponse{Alerts: []operator.VoyageAlert{{BookingRef: "OB-BKG-1042", Alert: alert}}}
			if errText != "" {
				resp.Alerts = append(resp.Alerts, operator.VoyageAlert{BookingRef: "OB-BKG-1043", Error: errText})
			}
		} else if status == http.StatusOK {
			resp.Alert = &intents.TnTAlertBody{AlertID: "AL-000009", Kind: "delay", Severity: "high", Summary: got.Reason,
				PreviousETA: "2026-10-13T08:00:00Z", NewETA: "2026-10-16T08:00:00Z", CauseEventIDs: []string{"EV-000008"}}
		}
		control.Answer(w, status, resp)
	}), nil)
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	cfg = filepath.Join(dir, "operator.yaml")
	if err := os.WriteFile(cfg, []byte(strings.Replace(operatorYAML, "SOCKET", socket, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg, got
}

func TestDisrupt(t *testing.T) {
	cfg, got := fakeNode(t, http.StatusOK, "")
	var out bytes.Buffer
	err := run([]string{"disrupt", "--config", cfg, "--booking", "OB-BKG-1042", "--delay", "72h", "--reason", "Engine repair."}, &out, &out)
	if err != nil {
		t.Fatal(err)
	}
	if got.BookingRef != "OB-BKG-1042" || got.Delay != "72h0m0s" || got.Reason != "Engine repair." {
		t.Errorf("sent %+v", got)
	}
	for _, want := range []string{"high delay alert AL-000009", "2026-10-13T08:00:00Z → 2026-10-16T08:00:00Z", "EV-000008"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// TestDisruptVoyage: --voyage disrupts every booking under way on it and
// lists each outcome; one that got no alert makes the command fail.
func TestDisruptVoyage(t *testing.T) {
	cfg, got := fakeNode(t, http.StatusOK, "")
	var out bytes.Buffer
	if err := run([]string{"disrupt", "--config", cfg, "--voyage", "42W", "--vessel", "Ocean Blue Aurora", "--delay", "24h", "--reason", "Strike."}, &out, &out); err != nil {
		t.Fatal(err)
	}
	if got.Voyage != "42W" || got.Vessel != "Ocean Blue Aurora" || got.BookingRef != "" {
		t.Errorf("sent %+v", got)
	}
	for _, want := range []string{"Disrupted voyage 42W: 1 booking(s)", "OB-BKG-1042: medium delay alert AL-000010"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	cfg, _ = fakeNode(t, http.StatusOK, "already 30 days late")
	out.Reset()
	err := run([]string{"disrupt", "--config", cfg, "--voyage", "42W", "--delay", "24h", "--reason", "Strike."}, &out, &out)
	if err == nil || !strings.Contains(out.String(), "OB-BKG-1043: no alert: already 30 days late") {
		t.Errorf("a partial voyage: %v\n%s", err, out.String())
	}
	cfg, _ = fakeNode(t, http.StatusNotFound, "no booking under way on that voyage")
	if err := run([]string{"disrupt", "--config", cfg, "--voyage", "43E", "--delay", "24h", "--reason", "Strike."}, &out, &out); err == nil {
		t.Error("an unknown voyage succeeded")
	}
}

func TestDisruptRejects(t *testing.T) {
	notFound, _ := fakeNode(t, http.StatusNotFound, "operator: no voyage in progress for that booking")
	var out bytes.Buffer
	if err := run([]string{"disrupt", "--config", notFound, "--booking", "OB-BKG-9", "--delay", "1h", "--reason", "Late."}, &out, &out); err == nil ||
		!strings.Contains(err.Error(), "no voyage") {
		t.Errorf("unknown booking: %v", err)
	}
	ok, _ := fakeNode(t, http.StatusOK, "")
	dir := filepath.Dir(ok)
	noControl := filepath.Join(dir, "nc.yaml")
	y, _ := os.ReadFile(ok)
	_ = os.WriteFile(noControl, []byte(strings.Split(string(y), "control:")[0]), 0o600)
	shipperCfg := filepath.Join(dir, "sh.yaml")
	_ = os.WriteFile(shipperCfg, []byte(`domain: cymbal-shoes.example
name: Cymbal
role: shipper
listen: ":8443"
tls: { mode: insecure-dev }
keys: [{ kid: cy-1, privateKeyFile: /nonexistent }]
control: { socket: /tmp/cy.sock }
`), 0o600)
	stopped := filepath.Join(dir, "stopped.yaml")
	_ = os.WriteFile(stopped, []byte(strings.Replace(operatorYAML, "SOCKET", filepath.Join(dir, "none.sock"), 1)), 0o600)
	for name, args := range map[string][]string{
		"no command":         {},
		"unknown command":    {"attack"},
		"no config":          {"disrupt", "--booking", "B", "--delay", "1h", "--reason", "Late."},
		"no booking":         {"disrupt", "--config", ok, "--delay", "1h", "--reason", "Late."},
		"booking and voyage": {"disrupt", "--config", ok, "--booking", "B", "--voyage", "42W", "--delay", "1h", "--reason", "Late."},
		"vessel alone":       {"disrupt", "--config", ok, "--booking", "B", "--vessel", "V", "--delay", "1h", "--reason", "Late."},
		"no delay":           {"disrupt", "--config", ok, "--booking", "B", "--reason", "Late."},
		"negative delay":     {"disrupt", "--config", ok, "--booking", "B", "--delay", "-1h", "--reason", "Late."},
		"no reason":          {"disrupt", "--config", ok, "--booking", "B", "--delay", "1h"},
		"missing config":     {"disrupt", "--config", "/nonexistent.yaml", "--booking", "B", "--delay", "1h", "--reason", "Late."},
		"no control":         {"disrupt", "--config", noControl, "--booking", "B", "--delay", "1h", "--reason", "Late."},
		"shipper's config":   {"disrupt", "--config", shipperCfg, "--booking", "B", "--delay", "1h", "--reason", "Late."},
		"node not running":   {"disrupt", "--config", stopped, "--booking", "B", "--delay", "1h", "--reason", "Late."},
		"bad flag":           {"disrupt", "--frobnicate"},
	} {
		if err := run(args, &out, &out); err == nil {
			t.Errorf("%s: succeeded", name)
		}
	}
}
