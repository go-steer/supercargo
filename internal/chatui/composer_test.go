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

// Derived from go-steer/core-tui@6e5af6c395503ca7b61e3ec61dc0d978fa9ab3ea
// (tui/composer_test.go, Copyright 2026 The go-steer team, Apache-2.0):
// the freshness and cost tests, driven through this package's model.

package chatui

import (
	"strings"
	"testing"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"

	"github.com/go-steer/supercargo/internal/ui/theme"
)

// TestComposerStaysFresh: after every step a session can take, the cached
// block is what the textarea draws now.
func TestComposerStaysFresh(t *testing.T) {
	m := newModel(t, nil, "dark")
	steps := []struct {
		name string
		do   func(m *Model)
	}{
		{"initial", func(*Model) {}},
		{"type a word", func(m *Model) { typeText(m, "hello there") }},
		{"backspace", func(m *Model) { m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace}) }},
		{"newline", func(m *Model) { m.Update(tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl}) }},
		{"second line", func(m *Model) { typeText(m, "and a second line") }},
		{"cursor up", func(m *Model) { m.Update(tea.KeyPressMsg{Code: tea.KeyUp}) }},
		{"narrow", func(m *Model) { m.Update(tea.WindowSizeMsg{Width: 40, Height: 24}) }},
		{"widen", func(m *Model) { m.Update(tea.WindowSizeMsg{Width: 160, Height: 50}) }},
		{"blur", func(m *Model) { m.input.Blur() }},
		{"focus", func(m *Model) { _ = m.input.Focus() }},
		{"clear to the placeholder", func(m *Model) { m.input.Reset() }},
		{"theme swap", func(m *Model) { m.setPalette(theme.Light) }},
		{"prompt glyph", func(m *Model) { m.input.SetPrompt("> ") }},
		{"long value that wraps", func(m *Model) {
			m.input.SetValue(strings.Repeat("a long prompt that has to wrap several times over. ", 6))
		}},
	}
	for _, step := range steps {
		step.do(m)
		if got, want := m.input.View(), m.input.ta.View(); got != want {
			t.Fatalf("after %q the cached block is stale\n cached:\n%s\n fresh:\n%s", step.name, got, want)
		}
	}
	if m.input.Height() != composerHeight || m.input.Width() == 0 || !m.input.Focused() || m.input.LineCount() == 0 {
		t.Error("forwarders")
	}
	_ = m.input.Cursor() // nil or not, depending on the cursor mode
}

// TestComposerRendersOncePerMutation: drawing an untouched composer is a
// field read.
func TestComposerRendersOncePerMutation(t *testing.T) {
	m := newModel(t, nil, "dark")
	if allocs := testing.AllocsPerRun(20, func() { _ = m.input.View() }); allocs != 0 {
		t.Errorf("drawing an untouched composer allocated %.0f objects", allocs)
	}
	var zero composer
	zero.SetPrompt("> ")
	zero.SetStyles(textarea.DefaultStyles(true))
	if zero.View() != "" {
		t.Error("an unconstructed composer rendered")
	}
}
