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
)

// TestFreight: a Brazilian truck leg's rate and ANTT pricing, and where
// its floor comes from, are checked; a floor to check needs a source.
func TestFreight(t *testing.T) {
	base := `domain: meridian-forwarding.example
name: Meridian Forwarding
roles: [operator, shipper]
listen: ":8443"
keys: [{ kid: mf-1, privateKeyFile: /keys/mf-1.pem }]
tls: { mode: local-ca, certFile: /certs/mf.pem, keyFile: /certs/mf-key.pem, caFile: /certs/ca.pem }
forwarder:
  refPrefix: MF
  freight:
    qualp: { url: "https://api.qualp.com.br", tokenEnv: QUALP_TOKEN }
  lanes:
    - from: NLRTM
      to: BRSAO
      legs:
        - { mode: ocean, carrier: ocean-blue.example, name: Ocean Blue, to: BRSSZ, facility: Santos Brasil Tecon terminal }
        - { mode: road, carrier: serra-transportes.example, name: Serra Transportes, to: BRSAO, freight: { rate: 1050, axles: 5, loadType: conteineirizada, table: A, from: "Santos, SP" } }
`
	c, err := Parse([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	if f := c.Forwarder.Lanes[0].Legs[1].Freight; f == nil || f.Rate != 1050 || f.Axles != 5 || f.From != "Santos, SP" || c.Forwarder.Freight.QualP.TokenEnv != "QUALP_TOKEN" {
		t.Errorf("%+v", f)
	}
	for name, edit := range map[string]string{
		"the mock on this machine": `url: "http://127.0.0.1:8090"`,
		"localhost":                `url: "http://localhost:8090/"`,
		"a label":                  `url: "https://api.qualp.com.br", label: "QualP (simulated)"`,
	} {
		in := strings.Replace(base, `url: "https://api.qualp.com.br"`, edit, 1)
		if _, err := Parse([]byte(in)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, edit := range map[string][2]string{
		"a ship with a floor":   {"to: BRSSZ, facility", "to: BRSSZ, freight: { rate: 1, axles: 5, loadType: geral, table: A }, facility"},
		"no rate":               {"rate: 1050, ", ""},
		"a negative rate":       {"rate: 1050", "rate: -1"},
		"a rate too high":       {"rate: 1050", "rate: 20000000"},
		"table E":               {"table: A", "table: E"},
		"an unknown load":       {"loadType: conteineirizada", "loadType: sand"},
		"containers on 2 axles": {"axles: 5", "axles: 2"},
		"eight axles":           {"axles: 5", "axles: 8"},
		"a from with a newline": {`from: "Santos, SP"`, `from: "Santos\nSP"`},
		"no source":             {"  freight:\n    qualp: { url: \"https://api.qualp.com.br\", tokenEnv: QUALP_TOKEN }\n", ""},
		"no qualp":              {"    qualp: { url: \"https://api.qualp.com.br\", tokenEnv: QUALP_TOKEN }\n", "    {}\n"},
		"http elsewhere":        {"https://api.qualp.com.br", "http://api.qualp.com.br"},
		"a user in the url":     {"https://api.qualp.com.br", "https://me@api.qualp.com.br"},
		"a query":               {"https://api.qualp.com.br", "https://api.qualp.com.br/?token=x"},
		"ftp":                   {"https://api.qualp.com.br", "ftp://api.qualp.com.br"},
		"no host":               {"https://api.qualp.com.br", "https://"},
		"a bad tokenEnv":        {"tokenEnv: QUALP_TOKEN", "tokenEnv: \"QUALP TOKEN\""},
		"a bad label":           {"tokenEnv: QUALP_TOKEN", "tokenEnv: QUALP_TOKEN, label: \"a\\u202eb\""},
		"no from at the port":   {`, from: "Santos, SP"`, ""},
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
