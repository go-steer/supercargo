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
)

func TestNewModel(t *testing.T) {
	if m, err := newModel(nil); m != nil || err != nil {
		t.Errorf("no llm section: %v, %v", m, err)
	}
	if m, err := newModel(&config.LLM{Provider: config.ProviderFake}); err != nil || m.Name() != "fake" {
		t.Errorf("fake: %v, %v", m, err)
	}
	// The key comes from the environment, and a node without it doesn't
	// start.
	t.Setenv("SUPERCARGO_TEST_KEY", "")
	if _, err := newModel(&config.LLM{Provider: config.ProviderAnthropic, APIKeyEnv: "SUPERCARGO_TEST_KEY"}); err == nil ||
		!strings.Contains(err.Error(), "SUPERCARGO_TEST_KEY") {
		t.Errorf("no key: %v", err)
	}
	t.Setenv("SUPERCARGO_TEST_KEY", "sk-test")
	m, err := newModel(&config.LLM{Provider: config.ProviderAnthropic, APIKeyEnv: "SUPERCARGO_TEST_KEY", Model: "claude-test"})
	if err != nil || m.Name() != "anthropic:claude-test" {
		t.Errorf("anthropic: %v, %v", m, err)
	}
}

func TestInquirySkills(t *testing.T) {
	all, err := inquirySkills(nil)
	if err != nil || len(all) != 1 || all[0].Name != "shipment-inquiry" || all[0].Source != "builtin" {
		t.Fatalf("built-in only: %+v, %v", all, err)
	}
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "delay-phrasing"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "delay-phrasing", "SKILL.md"), []byte("---\nname: delay-phrasing\ndescription: d\n---\nBe brief."), 0o644)
	all, err = inquirySkills(&config.Skills{Dir: dir})
	if err != nil || len(all) != 2 || all[1].Name != "delay-phrasing" || all[1].Source != "company" {
		t.Errorf("with company skills: %+v, %v", all, err)
	}
	// A broken company skill stops the node.
	_ = os.WriteFile(filepath.Join(dir, "delay-phrasing", "SKILL.md"), []byte("no frontmatter"), 0o644)
	if _, err := inquirySkills(&config.Skills{Dir: dir}); err == nil {
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
