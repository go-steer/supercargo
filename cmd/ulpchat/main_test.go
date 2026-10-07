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
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/go-steer/supercargo/internal/config"
)

// writeConfig writes a node configuration with extra appended, and
// returns its path.
func writeConfig(t *testing.T, role, extra string) string {
	t.Helper()
	dir := t.TempDir()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	key := filepath.Join(dir, "k.pem")
	if err := config.WriteSigningKey(key, priv); err != nil {
		t.Fatal(err)
	}
	y := "domain: cymbal-shoes.example\nname: Cymbal Shoes\nrole: " + role + "\nlisten: 127.0.0.1:1\ntls: { mode: insecure-dev }\n" +
		"keys: [{ kid: cy-1, privateKeyFile: \"" + key + "\" }]\n" + extra
	if role == config.RoleOperator {
		y += "operator: { refPrefix: OB, containerPrefix: OCBU, vessel: V, voyage: 1W, departureHour: 18, transit: 1h }\n"
	}
	path := filepath.Join(dir, "node.yaml")
	if err := os.WriteFile(path, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRun(t *testing.T) {
	var started tea.Model
	start := func(m tea.Model) error { started = m; return nil }
	dir := t.TempDir()
	biz := filepath.Join(dir, "business.yaml")
	_ = os.WriteFile(biz, []byte("shipments:\n  - shipperRef: P1\n    requiredBy: 2026-10-15\n"), 0o600)
	skillDir := filepath.Join(dir, "skills")
	_ = os.MkdirAll(filepath.Join(skillDir, "tone"), 0o755)
	_ = os.WriteFile(filepath.Join(skillDir, "tone", "SKILL.md"), []byte("---\nname: tone\ndescription: d\n---\nBe brief."), 0o600)
	good := writeConfig(t, config.RoleShipper, "control: { socket: /tmp/cy.sock }\nllm: { provider: fake }\n"+
		"business: { file: \""+biz+"\" }\nskills: { dir: \""+skillDir+"\" }\n")
	if err := run([]string{"--config", good, "--theme", "light"}, io.Discard, start); err != nil || started == nil {
		t.Fatalf("run: %v", err)
	}
	a, err := newAssistant(mustLoad(t, good))
	if err != nil {
		t.Fatal(err)
	}
	if a.Self.Endpoint != "http://cymbal-shoes.example/ulp" || a.Model.Name() != "fake" || len(a.Skills) != 3 ||
		a.Skills[2].Name != "tone" || !slices.Contains(a.Private, "2026-10-15") {
		t.Errorf("assistant %+v", a)
	}

	t.Setenv("SUPERCARGO_TEST_CHAT_KEY", "")
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"no config":   {nil, "--config is required"},
		"bad flag":    {[]string{"--nope"}, "not defined"},
		"bad theme":   {[]string{"--config", good, "--theme", "neon"}, "not dark or light"},
		"no file":     {[]string{"--config", filepath.Join(dir, "missing.yaml")}, "missing.yaml"},
		"an operator": {[]string{"--config", writeConfig(t, config.RoleOperator, "control: { socket: /tmp/ob.sock }\nllm: { provider: fake }\n")}, "shipper's chat"},
		"no control":  {[]string{"--config", writeConfig(t, config.RoleShipper, "llm: { provider: fake }\n")}, "no control socket"},
		"no llm":      {[]string{"--config", writeConfig(t, config.RoleShipper, "control: { socket: /tmp/cy.sock }\n")}, "no llm section"},
		"no key": {[]string{"--config", writeConfig(t, config.RoleShipper,
			"control: { socket: /tmp/cy.sock }\nllm: { provider: anthropic, apiKeyEnv: SUPERCARGO_TEST_CHAT_KEY }\n")}, "SUPERCARGO_TEST_CHAT_KEY"},
		"bad skills": {[]string{"--config", writeConfig(t, config.RoleShipper,
			"control: { socket: /tmp/cy.sock }\nllm: { provider: fake }\nskills: { dir: \""+dir+"\" }\n")}, "skill"},
	} {
		if err := run(c.args, io.Discard, start); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	t.Setenv("SUPERCARGO_TEST_CHAT_KEY", "sk-test")
	claude := writeConfig(t, config.RoleShipper, "control: { socket: /tmp/cy.sock }\nllm: { provider: anthropic, apiKeyEnv: SUPERCARGO_TEST_CHAT_KEY, model: claude-test }\n")
	if a, err := newAssistant(mustLoad(t, claude)); err != nil || a.Model.Name() != "anthropic:claude-test" {
		t.Errorf("anthropic: %v", err)
	}
}

func mustLoad(t *testing.T, path string) *config.Config {
	t.Helper()
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
