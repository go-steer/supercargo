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

// Package chatui is ulpchat's full-screen TUI: the shipper assistant's
// chat (internal/assistant), styled as a log the way mast-web is, in the
// shared grayscale themes with the Gemini aurora on model output.
package chatui

import (
	"context"
	"fmt"
	"image/color"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/go-steer/supercargo/internal/assistant"
	"github.com/go-steer/supercargo/internal/ui/theme"
)

// Options configure a Model.
type Options struct {
	Assistant *assistant.Assistant
	// Theme is "dark" or "light"; empty follows the terminal's background.
	Theme string
	// Node names the node's control socket, for the status bar.
	Node string
	Now  func() time.Time
}

// Role is who a transcript row is from.
type Role int

// Roles.
const (
	RoleYou Role = iota
	RoleAssistant
	RoleTool
	RoleSystem
)

// Row is one transcript entry.
type Row struct {
	Role Role
	At   time.Time
	Text string
	Tool *assistant.ToolEvent
	turn int64
}

// Model is the TUI's Bubble Tea model.
type Model struct {
	a          *assistant.Assistant
	conv       *assistant.Conversation
	node       string
	now        func() time.Time
	themeFixed bool
	st         styles

	rows     []Row
	pending  []assistant.DraftResult // drafts waiting for the dialog
	dialog   *assistant.DraftResult  // the open confirmation dialog
	dialogAt time.Time
	turn     atomic.Int64 // the turn in progress, for tool events
	vp       viewport.Model
	input    composer
	busy     bool
	events   chan tea.Msg
	ctx      context.Context // cancelled on quit: ends a turn in progress
	cancel   context.CancelFunc
	width    int
	height   int
	quitting bool
}

// Messages from the assistant's goroutine.
type (
	toolMsg struct {
		turn int64
		ev   assistant.ToolEvent
	}
	replyMsg struct {
		turn int64
		r    assistant.Reply
		err  error
	}
)

// composerHeight is the input's height in lines.
const composerHeight = 3

// New returns a Model. A's OnTool is taken over to show tool calls live.
func New(o Options) *Model {
	now := o.Now
	if now == nil {
		now = time.Now
	}
	p, fixed := theme.ByName(o.Theme)
	if !fixed {
		p = theme.Dark
	}
	m := &Model{a: o.Assistant, conv: &assistant.Conversation{}, node: o.Node, now: now, themeFixed: fixed,
		st: newStyles(p), events: make(chan tea.Msg, 64), vp: viewport.New()}
	m.ctx, m.cancel = context.WithCancel(context.Background())
	if m.a != nil {
		ctx, events := m.ctx, m.events
		m.a.OnTool = func(ev assistant.ToolEvent) {
			select {
			case events <- toolMsg{m.turn.Load(), ev}:
			case <-ctx.Done(): // nobody is reading any more
			}
		}
	}
	ta := textarea.New()
	ta.Placeholder = "Ask about a carrier, book a shipment, or check on one…"
	ta.Prompt = "▎ "
	ta.ShowLineNumbers = false
	ta.SetHeight(composerHeight)
	ta.SetStyles(m.st.composer())
	m.input = newComposer(ta)
	return m
}

// Init implements tea.Model.
func (m *Model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.input.Focus(), m.listen()}
	if !m.themeFixed {
		cmds = append(cmds, tea.RequestBackgroundColor)
	}
	return tea.Batch(cmds...)
}

func (m *Model) listen() tea.Cmd {
	ctx, events := m.ctx, m.events
	return func() tea.Msg {
		select {
		case msg := <-events:
			return msg
		case <-ctx.Done():
			return nil
		}
	}
}

// Update implements tea.Model.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		return m, nil
	case tea.BackgroundColorMsg:
		if !m.themeFixed {
			m.setPalette(theme.ForBackground(hex(msg.Color)))
		}
		return m, nil
	case toolMsg:
		m.addTool(msg)
		return m, m.listen()
	case replyMsg:
		m.busy = false
		switch {
		case msg.err != nil:
			m.add(Row{Role: RoleSystem, At: m.now(), Text: "The assistant failed: " + assistant.OneLine(msg.err.Error(), 300), turn: msg.turn})
		default:
			m.add(Row{Role: RoleAssistant, At: m.now(), Text: msg.r.Text, turn: msg.turn})
		}
		// A failed turn may still have made drafts: each opens the dialog.
		m.offer(msg.r.Drafts...)
		return m, nil
	case sentMsg:
		return m, m.sent(msg)
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" {
			m.quitting = true
			m.cancel()
			return m, tea.Quit
		}
		if m.dialog != nil {
			return m, m.dialogKey(msg)
		}
		switch msg.String() {
		case "enter":
			return m, m.send()
		case "ctrl+j":
			m.input, _ = m.input.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			return m, nil
		case "pgup", "pgdown":
			var cmd tea.Cmd
			m.vp, cmd = m.vp.Update(msg)
			return m, cmd
		}
	case tea.MouseWheelMsg:
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// send starts a turn with the composer's text.
func (m *Model) send() tea.Cmd {
	text := strings.TrimSpace(m.input.Value())
	if text == "" || m.busy || m.a == nil {
		return nil
	}
	if n := utf8.RuneCountInString(text); n > assistant.MaxUserText {
		// Kept in the composer, to shorten.
		m.add(Row{Role: RoleSystem, At: m.now(), Text: fmt.Sprintf("That message is %d characters; the limit is %d.", n, assistant.MaxUserText)})
		return nil
	}
	m.input.Reset()
	if strings.HasPrefix(text, "/") && m.command(text) {
		return nil
	}
	m.busy = true
	turn := m.turn.Add(1)
	m.add(Row{Role: RoleYou, At: m.now(), Text: text, turn: turn})
	a, conv, ctx := m.a, m.conv, m.ctx
	return func() tea.Msg {
		r, err := a.Turn(ctx, conv, text)
		return replyMsg{turn, r, err}
	}
}

func (m *Model) setPalette(p theme.Palette) {
	m.st = newStyles(p)
	m.input.SetStyles(m.st.composer())
	m.refresh()
}

// addTool adds a tool call's row: at the end, or, if its turn's reply
// overtook it (they come from different goroutines), before that reply.
func (m *Model) addTool(msg toolMsg) {
	r := Row{Role: RoleTool, At: m.now(), Tool: &msg.ev, turn: msg.turn}
	for i, row := range m.rows {
		if row.turn == msg.turn && row.Role != RoleYou && row.Role != RoleTool {
			m.rows = append(m.rows[:i], append([]Row{r}, m.rows[i:]...)...)
			m.refresh()
			return
		}
	}
	m.add(r)
}

func (m *Model) add(r Row) {
	m.rows = append(m.rows, r)
	m.refresh()
}

// layout sizes the parts: a header line and its rule, the transcript, a
// rule, the composer, and the status bar.
func (m *Model) layout() {
	m.vp.SetWidth(m.width)
	m.vp.SetHeight(max(1, m.height-composerHeight-4))
	m.input.SetWidth(max(10, m.width))
	m.refresh()
}

// refresh re-renders the transcript into the viewport, following the end.
func (m *Model) refresh() {
	if m.width == 0 {
		return
	}
	follow := m.vp.AtBottom() || m.vp.TotalLineCount() <= m.vp.Height()
	m.vp.SetContent(m.transcript())
	if follow {
		m.vp.GotoBottom()
	}
}

func (m *Model) transcript() string {
	if len(m.rows) == 0 {
		hint := m.st.dim.Render("Ask about a carrier by its domain, book a shipment, or check on one.")
		return lipgloss.Place(m.width, m.vp.Height(), lipgloss.Center, lipgloss.Center, hint)
	}
	var b strings.Builder
	for i, r := range m.rows {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(m.row(r))
	}
	return b.String()
}

// row renders one entry: a "ROLE: [hh:mm:ss]" header and the text under
// it, as mast-web's log does; a tool call is one line.
func (m *Model) row(r Row) string {
	stamp := m.st.dim.Render("[" + r.At.Format("15:04:05") + "]")
	body := lipgloss.NewStyle().Width(max(10, m.width-2))
	switch r.Role {
	case RoleTool:
		ev := r.Tool
		mark, what := m.st.ok.Render("✓"), m.st.dim.Render(toolSummary(ev))
		if ev.Error != "" {
			mark, what = m.st.bad.Render("✗"), m.st.bad.Render(assistant.OneLine(ev.Error, 120))
		}
		parts := []string{" ", m.st.dim.Render("▸"), m.st.tool.Render(ev.Name), mark}
		if what != "" && (ev.Error != "" || toolSummary(ev) != "") {
			parts = append(parts, what)
		}
		return strings.Join(append(parts, stamp), " ")
	case RoleAssistant:
		lines := strings.Split(body.Render(m.st.text.Render(r.Text)), "\n")
		var b strings.Builder
		b.WriteString(m.st.aurora("ASSISTANT:", true) + " " + stamp)
		for i, l := range lines {
			b.WriteString("\n" + m.st.edge(i, len(lines)) + " " + l)
		}
		return b.String()
	case RoleSystem:
		return m.st.pending.Render(body.Render(r.Text))
	}
	return m.st.you.Render("YOU:") + " " + stamp + "\n  " +
		strings.ReplaceAll(body.Render(m.st.text.Render(r.Text)), "\n", "\n  ")
}

// toolSummary is a tool call's input at a glance.
func toolSummary(ev *assistant.ToolEvent) string {
	if ev.Draft != nil {
		return "draft " + ev.Draft.DraftID
	}
	s := string(ev.Input)
	if s == "{}" {
		return ""
	}
	return assistant.OneLine(s, 60)
}

// View implements tea.Model.
func (m *Model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	v.WindowTitle = "ulpchat"
	return v
}

func (m *Model) render() string {
	if m.width == 0 || m.quitting {
		return ""
	}
	rule := m.st.rule.Render(strings.Repeat("─", m.width))
	body := m.vp.View()
	if m.dialog != nil {
		body = m.dialogView()
	}
	return strings.Join([]string{m.header(), rule, body, rule, m.input.View(), m.statusBar()}, "\n")
}

func (m *Model) header() string {
	who := ""
	if m.a != nil {
		who = m.st.dim.Render(" · " + m.a.Self.Name + " (" + m.a.Self.Domain + ")")
	}
	return m.st.aurora("supercargo", true) + m.st.dim.Render(" ulpchat") + who
}

func (m *Model) statusBar() string {
	left := " " + m.st.p.Name
	if m.a != nil && m.a.Model != nil {
		left = " model " + m.a.Model.Name() + " · node " + m.node + " ·" + left
	}
	if m.busy {
		left = m.st.pending.Render(" thinking…") + m.st.status.Render(left)
	} else {
		left = m.st.status.Render(left)
	}
	right := m.st.status.Render("enter send · ctrl+j newline · ctrl+c quit ")
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return lipgloss.NewStyle().MaxWidth(m.width).Render(left)
	}
	return left + m.st.status.Render(strings.Repeat(" ", gap)) + right
}

func hex(c color.Color) string {
	if c == nil {
		return ""
	}
	r, g, b, _ := c.RGBA()
	return fmt.Sprintf("#%02x%02x%02x", r>>8, g>>8, b>>8)
}
