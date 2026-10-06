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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	key := filepath.Join(dir, "key.pem")
	if _, err := runCmd(t, "", "keygen", "--kid", "cy-1", "--out", key); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "shipper.yaml")
	y := "domain: cymbal-shoes.example\nname: Cymbal\nrole: shipper\nlisten: \"127.0.0.1:0\"\ntls: { mode: insecure-dev }\n" +
		"keys: [{ kid: cy-1, privateKeyFile: \"" + key + "\" }]\n" +
		"peers: { ocean-blue.example: \"127.0.0.1:1\" }\n" // offline: a refused connection, not a DNS lookup
	if err := os.WriteFile(p, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const requestJSON = `{"shipperRef":"P1","origin":{"unLocode":"NLRTM","name":"Rotterdam"},"destination":{"unLocode":"USEWR","name":"Newark"},
"equipment":[{"isoType":"45G1","label":"40HC","count":1}],"cargo":{"description":"Shoes","hsCode":"6404"},"earliestDeparture":"2026-10-06"}`

func writeFile(t *testing.T, name, data string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestBookNeedsAYes: anything but "y" sends nothing.
func TestBookNeedsAYes(t *testing.T) {
	cfg := shipperConfig(t)
	req := writeFile(t, "req.json", requestJSON)
	for _, answer := range []string{"n\n", "\n", "yes please\n", ""} {
		out, err := runCmd(t, answer, "book", "--config", cfg, "--operator", "ocean-blue.example", "--request", req)
		if !errors.Is(err, errNotSent) || !strings.Contains(out, "Not sent.") {
			t.Errorf("answer %q: %q, %v; want errNotSent", answer, out, err)
		}
	}
}

// TestBookSummary: the person sees where updates go and who the consignee
// is before saying yes.
func TestBookSummary(t *testing.T) {
	cfg := shipperConfig(t)
	withAll := strings.Replace(requestJSON, `"earliestDeparture":"2026-10-06"`,
		`"earliestDeparture":"2026-10-06","consignee":{"name":"Cymbal Footwear","domain":"cymbal-footwear.example"},`+
			`"subscription":{"endpoint":"https://cymbal-shoes.example/ulp","intents":["ulp.tnt.event"]}`, 1)
	out, _ := runCmd(t, "n\n", "book", "--config", cfg, "--operator", "ocean-blue.example", "--request", writeFile(t, "r.json", withAll))
	for _, want := range []string{"cymbal-footwear.example", "https://cymbal-shoes.example/ulp", "ulp.tnt.event", "1 × 40HC", "Shoes", "NLRTM"} {
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
	for name, args := range map[string][]string{
		"no flags":          {"book"},
		"missing config":    {"book", "--config", filepath.Join(t.TempDir(), "none.yaml"), "--operator", "o.example", "--request", good},
		"operator config":   {"book", "--config", opCfg, "--operator", "o.example", "--request", good},
		"missing request":   {"book", "--config", cfg, "--operator", "o.example", "--request", filepath.Join(t.TempDir(), "none.json")},
		"unknown field":     {"book", "--config", cfg, "--operator", "o.example", "--request", writeFile(t, "r.json", strings.Replace(requestJSON, `"shipperRef"`, `"deadline":"x","shipperRef"`, 1))},
		"invalid body":      {"book", "--config", cfg, "--operator", "o.example", "--request", writeFile(t, "r.json", strings.Replace(requestJSON, "NLRTM", "nowhere", 1))},
		"not JSON":          {"book", "--config", cfg, "--operator", "o.example", "--request", writeFile(t, "r.json", "{")},
		"bad flag":          {"book", "--nope"},
		"unreachable, sent": {"book", "--config", cfg, "--operator", "ocean-blue.example", "--request", good, "--yes"},
	} {
		if _, err := runCmd(t, "", args...); err == nil {
			t.Errorf("%s: succeeded", name)
		}
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
	for name, args := range map[string][]string{
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
	for _, f := range []string{"text", "json"} {
		if _, err := logger(f, &bytes.Buffer{}); err != nil {
			t.Errorf("logger(%s): %v", f, err)
		}
	}
}
