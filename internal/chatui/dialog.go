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
	"errors"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/go-steer/supercargo/internal/assistant"
)

// The confirmation dialog is the only way a booking leaves the shipper
// (AGENTS.md: actions with external effect need a person's explicit
// yes). The model can make a draft, which opens the dialog; only a "y"
// typed into the open dialog sends it, through Assistant.Send. Nothing
// the model says reaches the dialog's keys.

// armDelay is how long the dialog ignores "y" after it opens, so a key
// typed for something else can't answer it.
const armDelay = 400 * time.Millisecond

// sentMsg is a send's outcome.
type sentMsg struct {
	id   string
	sent *assistant.Sent
	err  error
}

// offer queues drafts for the dialog and opens it if it's free.
func (m *Model) offer(ds ...assistant.DraftResult) {
	m.pending = append(m.pending, ds...)
	m.next()
}

// next opens the dialog on the next queued draft.
func (m *Model) next() {
	if m.dialog != nil || len(m.pending) == 0 {
		return
	}
	d := m.pending[0]
	m.pending = m.pending[1:]
	m.dialog, m.dialogAt = &d, m.now()
	m.input.Blur()
}

// close closes the dialog and opens the next (not while a send is in
// flight: its answer comes first), or gives the composer back.
func (m *Model) close() tea.Cmd {
	m.dialog = nil
	if !m.busy {
		m.next()
	}
	if m.dialog == nil {
		return m.input.Focus()
	}
	return nil
}

// dialogKey handles a key while the dialog is open: "y" sends, anything
// else (but ctrl+c, which quits) declines. For armDelay after it opens,
// and while a send is in flight, keys are ignored: they weren't typed as
// an answer to this dialog. A held key's repeats never count.
func (m *Model) dialogKey(msg tea.KeyPressMsg) tea.Cmd {
	d := m.dialog
	if m.busy || msg.IsRepeat || m.now().Sub(m.dialogAt) < armDelay {
		return nil
	}
	if msg.String() == "y" || msg.String() == "Y" {
		m.busy = true
		m.add(Row{Role: RoleSystem, At: m.now(), Text: "Sending " + d.DraftID + " to " + d.To + "…"})
		cmd := m.close()
		a, ctx, id := m.a, m.ctx, d.DraftID
		return tea.Batch(cmd, func() tea.Msg {
			s, err := a.Send(ctx, id)
			return sentMsg{id, s, err}
		})
	}
	m.add(Row{Role: RoleSystem, At: m.now(), Text: fmt.Sprintf(
		"Not sent. Draft %s stays open: type /send %s to review it again.", d.DraftID, d.DraftID)})
	return m.close()
}

// sent shows a send's outcome, and has the assistant tell the user about
// it.
func (m *Model) sent(msg sentMsg) tea.Cmd {
	if msg.err != nil {
		m.busy = false
		m.add(Row{Role: RoleSystem, At: m.now(), Text: "Draft " + msg.id + ": " + assistant.OneLine(msg.err.Error(), 300)})
		if errors.Is(msg.err, assistant.ErrOutcomeUnknown) {
			// The assistant must not think the draft is still unsent.
			_ = m.conv.Note("Sending draft " + msg.id + " failed with an unknown outcome: it may have reached the carrier. " +
				"The draft is gone; tell the user to check the shipments before booking again.")
		}
		m.next()
		if m.dialog == nil {
			return m.input.Focus()
		}
		return nil
	}
	c := msg.sent.Confirmation
	m.add(Row{Role: RoleSystem, At: m.now(), Text: fmt.Sprintf("Sent %s to %s, signed. Answer: %s %s.",
		msg.id, msg.sent.Draft.Operator, assistant.OneLine(c.Status, 40), assistant.OneLine(c.BookingRef, 64))})
	turn := m.turn.Add(1)
	a, conv, ctx, s := m.a, m.conv, m.ctx, msg.sent
	return func() tea.Msg {
		r, err := a.Report(ctx, conv, s)
		return replyMsg{turn, r, err}
	}
}

// command handles a composer line starting with "/".
func (m *Model) command(text string) bool {
	id, ok := strings.CutPrefix(text, "/send ")
	if !ok {
		return false
	}
	d, err := m.a.Drafts.Get(strings.TrimSpace(id))
	if err != nil {
		m.add(Row{Role: RoleSystem, At: m.now(), Text: "No open draft " + assistant.OneLine(id, 20) + "."})
		return true
	}
	m.offer(d.Result())
	return true
}

// dialogView is the open dialog: everything the carrier will learn, and
// what stays home.
func (m *Model) dialogView() string {
	d := m.dialog
	r := d.Request
	// Every field in full, wrapped: what the dialog shows is what is sent.
	width := max(30, min(90, m.width-8)) - 11
	line := func(label, value string) string {
		wrapped := lipgloss.NewStyle().Width(width).Render(assistant.OneLine(value, 1000))
		return m.st.dim.Render(fmt.Sprintf("%-11s", label)) + m.st.text.Render(strings.ReplaceAll(wrapped, "\n", "\n"+strings.Repeat(" ", 11)))
	}
	var eq []string
	for _, e := range r.Equipment {
		eq = append(eq, fmt.Sprintf("%d × %s", e.Count, e.Label))
	}
	lines := []string{
		line("Operator:", d.To),
		line("Route:", fmt.Sprintf("%s %s → %s %s", r.Origin.UNLocode, r.Origin.Name, r.Destination.UNLocode, r.Destination.Name)),
		line("Equipment:", strings.Join(eq, ", ")),
		line("Cargo:", fmt.Sprintf("%s, HS %s", r.Cargo.Description, r.Cargo.HSCode)),
		line("Departure:", "on/after "+r.EarliestDeparture),
		line("Reference:", r.ShipperRef),
	}
	if c := r.Consignee; c != nil {
		lines = append(lines, line("Consignee:", c.Name+" ("+c.Domain+")"))
	}
	if s := r.Subscription; s != nil {
		lines = append(lines, line("Updates:", strings.Join(s.Intents, ", ")+" → "+s.Endpoint))
	}
	if rb := d.NotSent["requiredBy"]; rb != "" {
		lines = append(lines, m.st.ok.Render(fmt.Sprintf("%-11s", "Not sent:"))+m.st.text.Render("your deadline, "+rb+" (it stays with us)"))
	}
	lines = append(lines, "", m.st.you.Render("Send this booking request?")+m.st.dim.Render(" [y/N]"))
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color(m.st.p.Blue)).Padding(0, 1).
		Render(strings.Join(lines, "\n"))
	title := m.st.you.Render(" Booking draft " + d.DraftID + " ")
	// The title sits in the top border, as in the design's mockup.
	if first, rest, ok := strings.Cut(box, "\n"); ok && lipgloss.Width(first) > lipgloss.Width(title)+4 {
		box = m.st.link.Render("╭─") + title + m.st.link.Render(strings.Repeat("─", lipgloss.Width(first)-lipgloss.Width(title)-3)+"╮") + "\n" + rest
	}
	return lipgloss.Place(m.width, m.vp.Height(), lipgloss.Center, lipgloss.Center, box)
}
