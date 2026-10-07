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

package chatui

import (
	"strings"

	"charm.land/bubbles/v2/textarea"
	"charm.land/lipgloss/v2"

	"github.com/go-steer/supercargo/internal/ui/theme"
)

// styles are the TUI's lipgloss styles, from one palette.
type styles struct {
	p       theme.Palette
	text    lipgloss.Style
	dim     lipgloss.Style
	rule    lipgloss.Style
	you     lipgloss.Style
	tool    lipgloss.Style
	ok      lipgloss.Style
	bad     lipgloss.Style
	pending lipgloss.Style
	link    lipgloss.Style
	status  lipgloss.Style
}

func newStyles(p theme.Palette) styles {
	c := lipgloss.Color
	return styles{
		p:       p,
		text:    lipgloss.NewStyle().Foreground(c(p.Text)),
		dim:     lipgloss.NewStyle().Foreground(c(p.Dim)),
		rule:    lipgloss.NewStyle().Foreground(c(p.Border)),
		you:     lipgloss.NewStyle().Foreground(c(p.Blue)).Bold(true),
		tool:    lipgloss.NewStyle().Foreground(c(p.Dim)).Bold(true),
		ok:      lipgloss.NewStyle().Foreground(c(p.Green)),
		bad:     lipgloss.NewStyle().Foreground(c(p.Red)),
		pending: lipgloss.NewStyle().Foreground(c(p.Yellow)).Italic(true),
		link:    lipgloss.NewStyle().Foreground(c(p.Blue)),
		status:  lipgloss.NewStyle().Foreground(c(p.Dim)).Background(c(p.Surface)),
	}
}

// aurora paints s with the Gemini gradient, one colour per character:
// for model output only.
func (st styles) aurora(s string, bold bool) string {
	runes := []rune(s)
	var b strings.Builder
	for i, r := range runes {
		t := 0.0
		if len(runes) > 1 {
			t = float64(i) / float64(len(runes)-1)
		}
		b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(theme.AuroraAt(t))).Bold(bold).Render(string(r)))
	}
	return b.String()
}

// edge is the aurora bar at the left of an assistant row's lines.
func (st styles) edge(line, of int) string {
	t := 0.0
	if of > 1 {
		t = float64(line) / float64(of-1)
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(theme.AuroraAt(t))).Render("▎")
}

func (st styles) composer() textarea.Styles {
	s := textarea.DefaultStyles(st.p.Dark)
	c := lipgloss.Color
	for _, state := range []*textarea.StyleState{&s.Focused, &s.Blurred} {
		state.Base = lipgloss.NewStyle()
		state.Text = lipgloss.NewStyle().Foreground(c(st.p.Text))
		state.Placeholder = lipgloss.NewStyle().Foreground(c(st.p.Dim))
		state.Prompt = lipgloss.NewStyle().Foreground(c(st.p.Blue))
		state.CursorLine = lipgloss.NewStyle()
	}
	return s
}
