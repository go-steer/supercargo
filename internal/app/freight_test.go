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
	"strings"
	"testing"

	"github.com/go-steer/supercargo/internal/config"
	"github.com/go-steer/supercargo/internal/qualp"
)

// TestFreightSource: a forwarder with forwarder.freight checks floors
// with QualP, its token from the environment; an empty token doesn't
// start.
func TestFreightSource(t *testing.T) {
	cert, key, ca := tlsFiles(t, "meridian-forwarding.example")
	cfg := func() *config.Config {
		c, err := config.Parse([]byte(`domain: meridian-forwarding.example
name: Meridian Forwarding
roles: [operator, shipper]
listen: "` + freePort(t) + `"
keys: [{ kid: mf-1, privateKeyFile: "` + keyFile(t) + `" }]
tls: { mode: local-ca, certFile: "` + cert + `", keyFile: "` + key + `", caFile: "` + ca + `" }
forwarder:
  refPrefix: MF
  freight: { qualp: { url: "http://127.0.0.1:8090", tokenEnv: TEST_QUALP_TOKEN, label: "QualP (simulated)" } }
`))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	t.Setenv("TEST_QUALP_TOKEN", "")
	if _, err := New(cfg(), quiet()); err == nil || !strings.Contains(err.Error(), "$TEST_QUALP_TOKEN is empty") {
		t.Errorf("an empty token: %v", err)
	}
	t.Setenv("TEST_QUALP_TOKEN", "demo")
	a, err := New(cfg(), quiet())
	if err != nil {
		t.Fatal(err)
	}
	c, ok := a.Orchestrator.Floors.(*qualp.Client)
	if !ok || c.Token != "demo" || c.Source != "QualP (simulated)" || c.BaseURL != "http://127.0.0.1:8090" {
		t.Errorf("floors %+v", a.Orchestrator.Floors)
	}
}
