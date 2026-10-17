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

package config

import (
	"strings"
	"testing"
	"time"
)

// TestDocks: a shipper's dock schedule needs its business file, a named
// site and docks, and a real time zone.
func TestDocks(t *testing.T) {
	base := `domain: cymbal-shoes.example
name: Cymbal Shoes
role: shipper
listen: ":8443"
keys: [{ kid: cy-1, privateKeyFile: /keys/cy-1.pem }]
tls: { mode: insecure-dev }
business: { file: /etc/ulp/business.yaml }
docks: { mock: "Agendamento (simulated)", site: Cajamar DC, zone: America/Sao_Paulo, docks: [Dock 1, Dock 2] }
`
	c, err := Parse([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	if d := c.Docks; d.Site != "Cajamar DC" || len(d.Docks) != 2 || d.Zone != "America/Sao_Paulo" {
		t.Errorf("%+v", d)
	}
	for name, edit := range map[string][2]string{
		"no business":    {"business: { file: /etc/ulp/business.yaml }\n", ""},
		"an operator":    {"role: shipper", "role: operator\noperator: { refPrefix: CY, road: { transit: 30h } }"},
		"no mock":        {`mock: "Agendamento (simulated)", `, ""},
		"no site":        {"site: Cajamar DC, ", ""},
		"no docks":       {", docks: [Dock 1, Dock 2]", ""},
		"nine docks":     {"docks: [Dock 1, Dock 2]", "docks: [a, b, c, d, e, f, g, h, i]"},
		"a long dock":    {"Dock 2]", strings.Repeat("x", 33) + "]"},
		"no zone":        {"zone: America/Sao_Paulo, ", ""},
		"a made-up zone": {"zone: America/Sao_Paulo", "zone: Mars/Olympus"},
		"local":          {"zone: America/Sao_Paulo", "zone: Local"},
		"a dock twice":   {"docks: [Dock 1, Dock 2]", "docks: [Dock 1, Dock 1]"},
		"a language":     {"tls: { mode: insecure-dev }", "tls: { mode: insecure-dev }\nllm: { provider: fake, language: fr }"},
	} {
		in := strings.Replace(base, edit[0], edit[1], 1)
		if in == base {
			t.Fatalf("%s: edit didn't apply", name)
		}
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestReceiverDocks: a receiving DC's schedule needs no business file;
// its hours and standing policy are checked.
func TestReceiverDocks(t *testing.T) {
	base := `domain: aurora.example
name: Aurora DC
roles: [receiver]
listen: ":8443"
keys: [{ kid: au-1, privateKeyFile: /keys/au-1.pem }]
tls: { mode: insecure-dev }
docks: { mock: "Agendamento (simulated)", site: Guarulhos DC, zone: America/Sao_Paulo, docks: [Dock 1, Dock 2], hours: [6, 10, 14, 18, 22], policy: { by: Receiving manager, unload: 1h, ahead: 36h } }
`
	c, err := Parse([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	if d := c.Docks; len(d.Hours) != 5 || d.Policy.By != "Receiving manager" || d.Policy.Ahead != 36*time.Hour {
		t.Errorf("%+v", d)
	}
	for name, edit := range map[string][2]string{
		"an hour of 24":      {"hours: [6, 10, 14, 18, 22]", "hours: [6, 24]"},
		"hours too close":    {"hours: [6, 10, 14, 18, 22]", "hours: [6, 8]"},
		"falling hours":      {"hours: [6, 10, 14, 18, 22]", "hours: [10, 6]"},
		"into the next day":  {"hours: [6, 10, 14, 18, 22]", "hours: [2, 6, 10, 14, 18, 23]"},
		"seven hours":        {"hours: [6, 10, 14, 18, 22]", "hours: [0, 4, 8, 12, 16, 20, 23]"},
		"a nameless policy":  {"by: Receiving manager, ", ""},
		"a long unload":      {"unload: 1h", "unload: 5h"},
		"a negative unload":  {"unload: 1h", "unload: -1h"},
		"a fortnight ahead":  {"ahead: 36h", "ahead: 336h"},
		"a policy's bidi":    {"by: Receiving manager", "by: \"a\u202eb\""},
		"no role for a dock": {"roles: [receiver]", "roles: [operator]\noperator: { refPrefix: AU, road: { transit: 30h } }"},
	} {
		in := strings.Replace(base, edit[0], edit[1], 1)
		if in == base {
			t.Fatalf("%s: edit didn't apply", name)
		}
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
