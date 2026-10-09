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

package directory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sample = `entries:
  - domain: ocean-blue.example
    name: Ocean Blue
    roles: [carrier]
    modes: [OCEAN]
    lanes: [{ from: NLRTM, to: USEWR }, { from: USEWR, to: NLRTM }]
    description: Transatlantic container service, weekly.
  - domain: north-star.example
    name: North Star Lines
    roles: [carrier]
    lanes: [{ from: DEHAM, to: USNYC }]
  - domain: anywhere.example
    name: Anywhere Freight
    roles: [carrier]
  - domain: cymbal-footwear.example
    name: Cymbal Footwear
    roles: [consignee]
`

func TestSearch(t *testing.T) {
	f, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	names := func(q Query) string {
		es, err := f.Search(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		var n []string
		for _, e := range es {
			n = append(n, e.Name)
		}
		return strings.Join(n, ", ")
	}
	for q, want := range map[Query]string{
		{}: "Ocean Blue, North Star Lines, Anywhere Freight, Cymbal Footwear",
		{Role: RoleCarrier, From: "NLRTM", To: "USEWR"}: "Ocean Blue, Anywhere Freight", // no lanes: any lane
		{Role: RoleCarrier, From: "DEHAM"}:              "North Star Lines, Anywhere Freight",
		{Role: RoleConsignee}:                           "Cymbal Footwear",
		{Text: "TRANSATLANTIC"}:                         "Ocean Blue",
		{Text: "north-star"}:                            "North Star Lines",
		{Role: RoleShipper}:                             "",
		{Mode: "OCEAN"}:                                 "Ocean Blue, North Star Lines, Anywhere Freight, Cymbal Footwear", // no modes: any mode
		{Mode: "ROAD"}:                                  "North Star Lines, Anywhere Freight, Cymbal Footwear",
	} {
		if got := names(q); got != want {
			t.Errorf("%+v: %q, want %q", q, got, want)
		}
	}
	if len(f.Entries()) != 4 {
		t.Errorf("entries: %d", len(f.Entries()))
	}
}

func TestSearchCaps(t *testing.T) {
	var b strings.Builder
	b.WriteString("entries:\n")
	for i := range MaxResults + 5 {
		b.WriteString("  - { domain: c" + string(rune('a'+i)) + ".example, name: C, roles: [carrier] }\n")
	}
	f, err := Parse([]byte(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	if es, _ := f.Search(context.Background(), Query{}); len(es) != MaxResults {
		t.Errorf("%d results", len(es))
	}
}

func TestParseRefuses(t *testing.T) {
	for name, in := range map[string]string{
		"unknown key":      "entries:\n  - { domain: a.example, name: A, roles: [carrier], trust: high }",
		"top-level key":    "carriers: []",
		"bad domain":       "entries:\n  - { domain: 'not a domain', name: A, roles: [carrier] }",
		"no name":          "entries:\n  - { domain: a.example, roles: [carrier] }",
		"blank name":       "entries:\n  - { domain: a.example, name: '  ', roles: [carrier] }",
		"bidi in name":     "entries:\n  - { domain: a.example, name: \"Ocean\\u202eBlue\", roles: [carrier] }",
		"newline in desc":  "entries:\n  - { domain: a.example, name: A, roles: [carrier], description: \"a\\nb\" }",
		"long name":        "entries:\n  - { domain: a.example, name: " + strings.Repeat("x", MaxName+1) + ", roles: [carrier] }",
		"no roles":         "entries:\n  - { domain: a.example, name: A }",
		"unknown role":     "entries:\n  - { domain: a.example, name: A, roles: [bank] }",
		"unknown mode":     "entries:\n  - { domain: a.example, name: A, roles: [carrier], modes: [ocean] }",
		"bad lane":         "entries:\n  - { domain: a.example, name: A, roles: [carrier], lanes: [{ from: rotterdam, to: USEWR }] }",
		"duplicate domain": "entries:\n  - { domain: a.example, name: A, roles: [carrier] }\n  - { domain: a.example, name: B, roles: [carrier] }",
		"two documents":    "entries: []\n---\nentries: []",
		"not yaml":         "entries: [",
	} {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	var b strings.Builder
	b.WriteString("entries:\n")
	for i := range MaxEntries + 1 {
		b.WriteString("  - { domain: c" + strings.Repeat("a", i%50+1) + string(rune('a'+i/50)) + ".example, name: C, roles: [carrier] }\n")
	}
	if _, err := Parse([]byte(b.String())); err == nil {
		t.Error("too many entries: accepted")
	}
	var lanes strings.Builder
	lanes.WriteString("entries:\n  - { domain: a.example, name: A, roles: [carrier], lanes: [")
	for range MaxLanes + 1 {
		lanes.WriteString("{ from: NLRTM, to: USEWR },")
	}
	lanes.WriteString("] }")
	if _, err := Parse([]byte(lanes.String())); err == nil {
		t.Error("too many lanes: accepted")
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "d.yaml")
	if err := os.WriteFile(p, []byte(sample), 0o600); err != nil {
		t.Fatal(err)
	}
	if f, err := Load(p); err != nil || len(f.Entries()) != 4 {
		t.Errorf("Load: %v", err)
	}
	if _, err := Load(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Error("a missing file loaded")
	}
	big := filepath.Join(dir, "big.yaml")
	if err := os.WriteFile(big, []byte("# "+strings.Repeat("x", MaxFile)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(big); err == nil {
		t.Error("an oversized file loaded")
	}
}

func TestNameKey(t *testing.T) {
	for in, want := range map[string]string{
		"Ocean Blue":           "ocean blue",
		"  Ocean   Blue, Inc.": "ocean blue inc",
		"OCEAN-BLUE":           "ocean blue",
		"Müller & Söhne":       "müller söhne",
		"!!!":                  "",
	} {
		if got := NameKey(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
