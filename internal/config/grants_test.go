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

// TestGrants: a receiver lists the issuers whose grants it keeps.
func TestGrants(t *testing.T) {
	base := "domain: aurora.example\nname: Aurora\nroles: [receiver]\nlisten: \":8443\"\nkeys: [{ kid: a-1, privateKeyFile: /k.pem }]\ntls: { mode: insecure-dev }\ngrants: { from: [vale-verde.example] }\n"
	c, err := Parse([]byte(base))
	if err != nil || len(c.Grants.From) != 1 {
		t.Fatalf("%+v %v", c, err)
	}
	for name, edit := range map[string][2]string{
		"an operator":  {"roles: [receiver]", "roles: [operator]\noperator: { refPrefix: A, road: { transit: 30h } }"},
		"empty":        {"from: [vale-verde.example]", "from: []"},
		"not a domain": {"from: [vale-verde.example]", "from: [Vale Verde]"},
		"ourselves":    {"from: [vale-verde.example]", "from: [aurora.example]"},
		"too many":     {"from: [vale-verde.example]", "from: [" + strings.Repeat("a.example, ", MaxGrantIssuers) + "b.example]"},
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
