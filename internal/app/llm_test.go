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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/supercargo/internal/config"
	"github.com/go-steer/supercargo/internal/operator"
)

func TestNewModel(t *testing.T) {
	if m, err := NewModel(nil, nil); m != nil || err != nil {
		t.Errorf("no llm section: %v, %v", m, err)
	}
	if m, err := NewModel(&config.LLM{Provider: config.ProviderFake}, operator.FakeInquiryModel()); err != nil || m.Name() != "fake" {
		t.Errorf("fake: %v, %v", m, err)
	}
	// The key comes from the environment, and a node without it doesn't
	// start.
	t.Setenv("SUPERCARGO_TEST_KEY", "")
	if _, err := NewModel(&config.LLM{Provider: config.ProviderAnthropic, APIKeyEnv: "SUPERCARGO_TEST_KEY"}, nil); err == nil ||
		!strings.Contains(err.Error(), "SUPERCARGO_TEST_KEY") {
		t.Errorf("no key: %v", err)
	}
	t.Setenv("SUPERCARGO_TEST_KEY", "sk-test")
	m, err := NewModel(&config.LLM{Provider: config.ProviderAnthropic, APIKeyEnv: "SUPERCARGO_TEST_KEY", Model: "claude-test"}, nil)
	if err != nil || m.Name() != "anthropic:claude-test" {
		t.Errorf("anthropic: %v, %v", m, err)
	}
	gem := &config.LLM{Provider: config.ProviderGemini, APIKeyEnv: "SUPERCARGO_TEST_KEY"}
	if _, err := NewModel(gem, nil); err == nil {
		t.Error("gemini with no model started")
	}
	gem.Model = "gemini-test"
	if m, err := NewModel(gem, nil); err != nil || m.Name() != "gemini:gemini-test" {
		t.Errorf("gemini: %v, %v", m, err)
	}
}

// TestNewModelVertex: on Vertex AI the project and location come from the
// environment and the credentials from Application Default Credentials (a
// file here, never the metadata server), with no key anywhere.
func TestNewModelVertex(t *testing.T) {
	dir := t.TempDir()
	adc := filepath.Join(dir, "adc.json")
	if err := os.WriteFile(adc, []byte(`{"type":"authorized_user","client_id":"c","client_secret":"s","refresh_token":"r"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dir)
	t.Setenv("CLOUDSDK_CONFIG", dir)
	t.Setenv("GCE_METADATA_HOST", "127.0.0.1:1")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", adc)
	t.Setenv("SUPERCARGO_TEST_PROJECT", "")
	t.Setenv("SUPERCARGO_TEST_LOCATION", "")
	vertex := func(provider, model string) *config.LLM {
		return &config.LLM{Provider: provider, Backend: config.BackendVertex, Model: model,
			ProjectEnv: "SUPERCARGO_TEST_PROJECT", LocationEnv: "SUPERCARGO_TEST_LOCATION"}
	}
	if _, err := NewModel(vertex(config.ProviderGemini, "g"), nil); err == nil || !strings.Contains(err.Error(), "SUPERCARGO_TEST_PROJECT") {
		t.Errorf("no project: %v", err)
	}
	t.Setenv("SUPERCARGO_TEST_PROJECT", "proj")
	if _, err := NewModel(vertex(config.ProviderGemini, "g"), nil); err == nil || !strings.Contains(err.Error(), "SUPERCARGO_TEST_LOCATION") {
		t.Errorf("no location: %v", err)
	}
	t.Setenv("SUPERCARGO_TEST_LOCATION", "us-central1")
	if _, err := NewModel(vertex(config.ProviderGemini, ""), nil); err == nil {
		t.Error("gemini with no model started")
	}
	if m, err := NewModel(vertex(config.ProviderGemini, "g"), nil); err != nil || m.Name() != "vertex:gemini:g" {
		t.Errorf("gemini: %v, %v", m, err)
	}
	if m, err := NewModel(vertex(config.ProviderAnthropic, "c"), nil); err != nil || m.Name() != "vertex:anthropic:c" {
		t.Errorf("claude: %v, %v", m, err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", filepath.Join(dir, "missing.json"))
	if _, err := NewModel(vertex(config.ProviderAnthropic, "c"), nil); err == nil {
		t.Error("claude with no credentials started")
	}
}

// TestPartners: the assistant gets the address book and the directory;
// a broken file of either stops the node and the assistant.
func TestPartners(t *testing.T) {
	dir := t.TempDir()
	book, search := filepath.Join(dir, "book.yaml"), filepath.Join(dir, "dir.yaml")
	_ = os.WriteFile(book, []byte("entries:\n  - { domain: cymbal-footwear.example, name: Cymbal Footwear, roles: [consignee] }\n"), 0o600)
	_ = os.WriteFile(search, []byte("entries:\n  - { domain: ocean-blue.example, name: Ocean Blue, roles: [carrier] }\n"), 0o600)
	c := cfg(t, config.RoleShipper)
	c.LLM = &config.LLM{Provider: config.ProviderFake}
	if a, err := ShipperAssistant(c, nil); err != nil || a.AddressBook != nil || a.Directory != nil {
		t.Errorf("neither configured: %+v, %v", a, err)
	}
	c.AddressBook, c.Directory = &config.AddressBook{File: book}, &config.Directory{File: search}
	a, err := ShipperAssistant(c, nil)
	if err != nil || len(a.AddressBook) != 1 || a.AddressBook[0].Name != "Cymbal Footwear" || a.Directory == nil {
		t.Fatalf("both: %+v, %v", a, err)
	}
	for _, broken := range []func(){
		func() { c.AddressBook.File = filepath.Join(dir, "missing.yaml") },
		func() { c.AddressBook.File = book; c.Directory.File = book + ".missing" },
	} {
		broken()
		if _, err := ShipperAssistant(c, nil); err == nil {
			t.Error("an assistant with a broken partner file")
		}
		if _, err := New(c, quiet()); err == nil {
			t.Error("a node with a broken partner file started")
		}
	}
}

func TestAgentSkills(t *testing.T) {
	all, err := agentSkills("shipment-inquiry", nil)
	if err != nil || len(all) != 1 || all[0].Name != "shipment-inquiry" || all[0].Source != "builtin" {
		t.Fatalf("built-in only: %+v, %v", all, err)
	}
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "delay-phrasing"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "delay-phrasing", "SKILL.md"), []byte("---\nname: delay-phrasing\ndescription: d\n---\nBe brief."), 0o644)
	all, err = agentSkills("shipment-inquiry", &config.Skills{Dir: dir})
	if err != nil || len(all) != 2 || all[1].Name != "delay-phrasing" || all[1].Source != "company" {
		t.Errorf("with company skills: %+v, %v", all, err)
	}
	// A broken company skill stops the node.
	_ = os.WriteFile(filepath.Join(dir, "delay-phrasing", "SKILL.md"), []byte("no frontmatter"), 0o644)
	if _, err := agentSkills("shipment-inquiry", &config.Skills{Dir: dir}); err == nil {
		t.Error("a broken company skill loaded")
	}
	c := cfg(t, config.RoleOperator)
	c.Skills = &config.Skills{Dir: dir}
	if _, err := New(c, quiet()); err == nil {
		t.Error("New started an operator with a broken company skill")
	}
	c.Skills, c.LLM = nil, &config.LLM{Provider: config.ProviderAnthropic, APIKeyEnv: "SUPERCARGO_UNSET_KEY_FOR_TEST"}
	if _, err := New(c, quiet()); err == nil {
		t.Error("New started an operator configured for Claude without a key")
	}
}
