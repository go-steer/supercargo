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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/supercargo/internal/assistant"
	"github.com/go-steer/supercargo/internal/config"
)

// TestDocksWiring: a shipper with docks seeds its schedule from the
// business file, serves it on the control API, and gives its own
// assistant (not an outside agent's) the dock tools.
func TestDocksWiring(t *testing.T) {
	dir := t.TempDir()
	biz := filepath.Join(dir, "business.yaml")
	if err := os.WriteFile(biz, []byte("shipments:\n  - { shipperRef: CY-PO-55120, receivingSlot: { date: 2099-01-10, window: \"08:00-12:00\", warehouse: dock 2 } }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	parse := func(window string) (*config.Config, error) {
		_ = os.WriteFile(biz, []byte("shipments:\n  - { shipperRef: CY-PO-55120, receivingSlot: { date: 2099-01-10, window: \""+window+"\", warehouse: dock 2 } }\n"), 0o600)
		return config.Parse([]byte(`domain: cymbal-shoes.example
name: Cymbal Shoes
role: shipper
listen: "127.0.0.1:0"
keys: [{ kid: cy-1, privateKeyFile: "` + keyFile(t) + `" }]
tls: { mode: insecure-dev }
llm: { provider: fake }
business: { file: "` + biz + `" }
docks: { mock: "Docks (simulated)", site: Cajamar DC, zone: America/Sao_Paulo, docks: [Dock 1, Dock 2] }
`))
	}
	cfg, err := parse("08:00-12:00")
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(cfg, quiet())
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	a.controlAPI.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/docks/appointments/CY-PO-55120", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"dock":"Dock 2","start":"2099-01-10T08:00:00-03:00"`) {
		t.Errorf("HTTP %d %s", rec.Code, rec.Body)
	}
	own, err := ShipperAssistant(cfg, assistant.ControlNode{})
	if err != nil || own.Docks == nil {
		t.Errorf("the own assistant: %v, docks %v", err, own != nil && own.Docks != nil)
	}
	outside, err := ToolAssistant(cfg, assistant.ControlNode{})
	if err != nil || outside.Docks != nil {
		t.Errorf("an outside agent's: %v", err)
	}
	// A slot window the schedule can't seed stops the node.
	bad, err := parse("12:00-08:00")
	if err == nil {
		if _, err := New(bad, quiet()); err == nil {
			t.Error("a backwards window started")
		}
	}
}
