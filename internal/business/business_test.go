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

package business

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const example = `
shipments:
  - shipperRef: CY-PO-55120
    requiredBy: 2026-10-15
    receivingSlot: { date: 2026-10-15, window: "08:00-12:00", warehouse: "Newark DC <dock 4> & annex" }
    notes: "Spring launch stock: shelves must be full by the 16th"
  - shipperRef: CY-PO-55121
notes:
  - "Escalate delays over 48h to the launch team"
  - short
`

func TestParse(t *testing.T) {
	c, err := Parse([]byte(example))
	if err != nil {
		t.Fatal(err)
	}
	s, ok := c.For("CY-PO-55120")
	if !ok || s.RequiredBy != "2026-10-15" || s.ReceivingSlot.Window != "08:00-12:00" {
		t.Errorf("For: %+v %v", s, ok)
	}
	if _, ok := c.For("CY-PO-9"); ok {
		t.Error("For found an unknown reference")
	}
	if _, ok := (*Context)(nil).For("x"); ok {
		t.Error("a nil context found something")
	}
	// Secrets: free text of at least GuardMinLen characters; never dates.
	secrets := c.Secrets()
	if len(secrets) != 4 || strings.Contains(strings.Join(secrets, "|"), "2026") {
		t.Errorf("secrets %q", secrets)
	}
	if (*Context)(nil).Secrets() != nil || (*Context)(nil).Private() != nil {
		t.Error("a nil context has secrets")
	}
	// Private: the secrets and the dates.
	if p := c.Private(); len(p) != 6 || p[4] != "2026-10-15" || p[5] != "2026-10-15" {
		t.Errorf("private %q", p)
	}
	if c, err := Parse(nil); err != nil || len(c.Shipments) != 0 {
		t.Errorf("an empty file: %+v, %v", c, err)
	}
}

func TestGuard(t *testing.T) {
	c, _ := Parse([]byte(example))
	guard := c.Guard()
	body := func(v any) []byte { b, _ := json.Marshal(v); return b }
	for name, b := range map[string][]byte{
		"a note in a question": body(map[string]string{"question": "Will shelves be full? Spring launch stock: shelves must be full by the 16th"}),
		// encoding/json escapes <, > and &: the guard looks for that form too.
		"an escaped warehouse": body(map[string]string{"question": "Deliver to Newark DC <dock 4> & annex"}),
		"a general note":       body(map[string]string{"x": "Escalate delays over 48h to the launch team"}),
		"the receiving window": body(map[string]string{"question": "Can it arrive 08:00-12:00 on Thursday?"}),
	} {
		if err := guard(b); !errors.Is(err, ErrLeak) {
			t.Errorf("%s: %v, want ErrLeak", name, err)
		}
	}
	for name, b := range map[string][]byte{
		"a date":        body(map[string]string{"earliestDeparture": "2026-10-15"}),
		"a short note":  body(map[string]string{"x": "short"}),
		"nothing of it": body(map[string]string{"question": "Where is OB-BKG-1042?"}),
	} {
		if err := guard(b); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestGuardWithoutHTMLEscaping: a body written without encoding/json's
// HTML escaping (quotes still escaped, < > & raw) is caught too.
func TestGuardWithoutHTMLEscaping(t *testing.T) {
	c, err := Parse([]byte(`notes: ["Ship \"fast\" <now> & then"]`))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(map[string]string{"question": "Please Ship \"fast\" <now> & then"})
	if err := c.Guard()(buf.Bytes()); !errors.Is(err, ErrLeak) {
		t.Errorf("an unescaped body: %v, want ErrLeak (%s)", err, buf.Bytes())
	}
}

func TestParseRejects(t *testing.T) {
	for name, text := range map[string]string{
		"unknown key":       "shipments: []\nsecret: x\n",
		"bad ref":           "shipments: [{shipperRef: 'a/b'}]\n",
		"duplicate ref":     "shipments: [{shipperRef: A}, {shipperRef: A}]\n",
		"bad requiredBy":    "shipments: [{shipperRef: A, requiredBy: 'next week'}]\n",
		"bad slot date":     "shipments: [{shipperRef: A, receivingSlot: {date: 15-10-2026}}]\n",
		"bad window":        "shipments: [{shipperRef: A, receivingSlot: {date: 2026-10-15, window: morning}}]\n",
		"control in notes":  "shipments: [{shipperRef: A, notes: \"a\\u001bb\"}]\n",
		"bidi in warehouse": "shipments: [{shipperRef: A, receivingSlot: {date: 2026-10-15, warehouse: \"a\\u202eb\"}}]\n",
		"long note":         "notes: [\"" + strings.Repeat("n", MaxText+1) + "\"]\n",
		"not YAML":          "shipments: [\n",
		"two documents":     "notes: [a]\n---\nbogus: 1\n",
		"impossible window": "shipments: [{shipperRef: A, receivingSlot: {date: 2026-10-15, window: \"99:99-00:00\"}}]\n",
	} {
		if _, err := Parse([]byte(text)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
	var many strings.Builder
	many.WriteString("shipments:\n")
	for i := range MaxShipments + 1 {
		many.WriteString("  - shipperRef: R" + strings.Repeat("x", i%3) + string(rune('A'+i%26)) + "-" + itoa(i) + "\n")
	}
	if _, err := Parse([]byte(many.String())); err == nil {
		t.Error("more than MaxShipments parsed")
	}
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "business.yaml")
	_ = os.WriteFile(p, []byte(example), 0o600)
	if c, err := Load(p); err != nil || len(c.Shipments) != 2 {
		t.Errorf("Load: %v", err)
	}
	if _, err := Load(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Error("a missing file loaded")
	}
	big := filepath.Join(dir, "big.yaml")
	_ = os.WriteFile(big, []byte("notes: []\n#"+strings.Repeat("x", MaxFile)), 0o600)
	if _, err := Load(big); err == nil {
		t.Error("an oversized file loaded")
	}
}
