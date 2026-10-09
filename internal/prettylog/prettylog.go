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

// Package prettylog is the node log for people (`ulpnode serve
// --log-format pretty`): a slog.Handler that renders the node's records,
// the same ones the JSON log carries (internal/activity's SlogSink), as
// one readable line each, coloured with the shared theme tokens.
//
//	15:04:05 ← ulp.tnt.inquiry            cymbal-shoes.example   a2a  VERIFIED (kid cy-demo-1)  ALLOWED
//	15:04:05 ✦ model fake  12ms  120→40 tokens  ok
//
// Green is verified and accepted, red rejected or denied, yellow alerts
// and warnings; model output gets the Gemini aurora.
package prettylog

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode"

	"charm.land/lipgloss/v2"

	"github.com/go-steer/supercargo/internal/ui/theme"
)

// Options configure a Handler.
type Options struct {
	// Color turns on ANSI colour (off for files, pipes and NO_COLOR).
	Color bool
	// Palette is the theme (default theme.Dark).
	Palette *theme.Palette
	// Level is the least level written (default Info).
	Level slog.Level
}

// Handler is the pretty slog.Handler. Safe for concurrent use.
type Handler struct {
	mu    *sync.Mutex
	w     io.Writer
	o     Options
	p     theme.Palette
	attrs []slog.Attr
}

// New returns a Handler writing to w.
func New(w io.Writer, o Options) *Handler {
	p := theme.Dark
	if o.Palette != nil {
		p = *o.Palette
	}
	return &Handler{mu: &sync.Mutex{}, w: w, o: o, p: p}
}

// Enabled implements slog.Handler.
func (h *Handler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.o.Level }

// WithAttrs implements slog.Handler.
func (h *Handler) WithAttrs(as []slog.Attr) slog.Handler {
	c := *h
	c.attrs = append(append([]slog.Attr(nil), h.attrs...), as...)
	return &c
}

// WithGroup implements slog.Handler. Groups are flattened: the node
// doesn't use them.
func (h *Handler) WithGroup(string) slog.Handler { return h }

// record is one log record's attributes, as text.
type record map[string]string

func (r record) get(k string) string { return r[k] }

// Handle implements slog.Handler.
func (h *Handler) Handle(_ context.Context, r slog.Record) error {
	rec := record{}
	var order []string
	add := func(a slog.Attr) bool {
		if a.Equal(slog.Attr{}) {
			return true
		}
		v := a.Value.Resolve()
		var s string
		if v.Kind() == slog.KindAny {
			if ss, ok := v.Any().([]string); ok {
				s = strings.Join(ss, ",")
			} else {
				s = fmt.Sprint(v.Any())
			}
		} else {
			s = v.String()
		}
		if _, seen := rec[a.Key]; !seen {
			order = append(order, a.Key)
		}
		rec[a.Key] = safe(s)
		return true
	}
	for _, a := range h.attrs {
		add(a)
	}
	r.Attrs(add)
	line := h.render(r, safe(r.Message), rec, order)
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, line+"\n")
	return err
}

// maxField caps one attribute's text.
const maxField = 240

// safe makes a value fit for one line on a terminal: log text includes
// partner data. Control and format characters go, runs of white space
// become one space (so padding can't push text onto a line of its own),
// and it's cut at maxField characters.
func safe(s string) string {
	s = strings.Join(strings.FieldsFunc(strings.Map(func(r rune) rune {
		if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Co, unicode.Cs) && !unicode.IsSpace(r) || r == 0x2028 || r == 0x2029 {
			return -1
		}
		return r
	}, s), unicode.IsSpace), " ")
	if r := []rune(s); len(r) > maxField {
		s = string(r[:maxField-1]) + "…"
	}
	return s
}

// paint colours s with hex, if colour is on.
func (h *Handler) paint(hex, s string, bold bool) string {
	if !h.o.Color || s == "" {
		return s
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(hex)).Bold(bold).Render(s)
}

// aurora paints s with the gradient, if colour is on.
func (h *Handler) aurora(s string) string {
	if !h.o.Color {
		return s
	}
	runes := []rune(s)
	var b strings.Builder
	for i, r := range runes {
		t := 0.0
		if len(runes) > 1 {
			t = float64(i) / float64(len(runes)-1)
		}
		b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(theme.AuroraAt(t))).Render(string(r)))
	}
	return b.String()
}

func (h *Handler) render(r slog.Record, msg string, rec record, order []string) string {
	stamp := h.paint(h.p.Dim, "--:--:--", false)
	if !r.Time.IsZero() {
		stamp = h.paint(h.p.Dim, r.Time.Format(time.TimeOnly), false)
	}
	switch msg {
	case "envelope":
		return stamp + " " + h.envelope(rec)
	case "model call":
		return stamp + " " + h.model(rec)
	case "alert summary":
		text := fmt.Sprintf("✦ summary %s  %s: %s", rec.get("booking"), strings.ToUpper(rec.get("impact")), rec.get("text"))
		if rec.get("generatedBy") == "template" {
			text += "  (template)"
		}
		line := stamp + " " + h.aurora(text)
		if u := rec.get("unheldCauses"); u != "" {
			// The operator cited events this shipper never received signed.
			line += "  " + h.paint(h.p.Red, "! cited but not received: "+u, true)
		}
		return line
	case "event recorded", "event published":
		parts := []string{rec.get("booking"), rec.get("event"), rec.get("location"), join([]string{rec.get("container"), rec.get("vessel")}),
			rec.get("at"), h.paint(h.p.Dim, rec.get("eventId")+" ("+strings.TrimPrefix(msg, "event ")+")", false)}
		return stamp + " · " + join(parts)
	case "alert recorded", "alert published":
		eta := ""
		if rec.get("newEta") != "" {
			eta = fmt.Sprintf("  ETA %s → %s", rec.get("previousEta"), rec.get("newEta"))
		}
		if c := rec.get("causeEventIds"); c != "" {
			eta += "  causes " + c
		}
		return stamp + " " + h.paint(h.p.Yellow, fmt.Sprintf("! alert %s %s  %s %s %s: %s%s", strings.TrimPrefix(msg, "alert "),
			rec.get("booking"), rec.get("severity"), rec.get("kind"), rec.get("alertId"), rec.get("summary"), eta), true)
	}
	var b strings.Builder
	b.WriteString(stamp + " ")
	switch {
	case r.Level >= slog.LevelError:
		b.WriteString(h.paint(h.p.Red, "ERROR ", true))
	case r.Level >= slog.LevelWarn:
		b.WriteString(h.paint(h.p.Yellow, "WARN ", true))
	}
	b.WriteString(msg)
	for _, k := range order {
		if v := rec[k]; v != "" {
			b.WriteString("  " + h.paint(h.p.Dim, k+"=", false) + v)
		}
	}
	return b.String()
}

func join(parts []string) string {
	var out []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "  ")
}

// envelope renders an envelope decision: direction, intent, peer,
// binding, and what the node concluded.
func (h *Handler) envelope(rec record) string {
	in := rec.get("dir") == "in"
	arrow, peer := "→", rec.get("receiver")
	if in {
		arrow, peer = "←", rec.get("sender")
	}
	kid := ""
	if rec.get("kid") != "" {
		kid = " (kid " + rec.get("kid") + ")"
	}
	why := rec.get("reason")
	if c := rec.get("code"); c != "" {
		why = c + ": " + why
	}
	// VERIFIED only when the node says it verified; DENIED only for an
	// authorization refusal; anything else a verified message was refused
	// for (stale, replayed, an invalid body) is REFUSED.
	verified := rec.get("verified") == "true"
	var status string
	switch outcome := rec.get("outcome"); {
	case outcome == "accepted" && verified:
		status = h.paint(h.p.Green, "VERIFIED"+kid, true) + "  " + h.paint(h.p.Green, "ALLOWED", false)
	case outcome == "response" && verified:
		status = h.paint(h.p.Green, "VERIFIED"+kid, true) + "  reply"
	case outcome == "sent":
		status = h.paint(h.p.Dim, "signed"+kid, false)
	case outcome == "rejected" && verified && rec.get("code") == "forbidden":
		status = h.paint(h.p.Green, "VERIFIED"+kid, true) + "  " + h.paint(h.p.Red, "DENIED "+why, true)
	case (outcome == "rejected" || outcome == "failed") && verified && rec.get("dir") == "in":
		status = h.paint(h.p.Green, "VERIFIED"+kid, true) + "  " + h.paint(h.p.Red, "REFUSED "+why, true)
	case outcome == "rejected":
		status = h.paint(h.p.Red, "REJECTED "+why, true)
	default: // malformed, failed, or an outcome claiming success unverified
		status = h.paint(h.p.Red, strings.ToUpper(outcome)+" "+why, true)
	}
	via := rec.get("via")
	if via == "" {
		via = "-" // not known (a message that didn't parse)
	}
	if id := rec.get("messageId"); id != "" {
		status += "  " + h.paint(h.p.Dim, id, false)
	}
	intent := rec.get("intent")
	if intent == "" {
		intent = "(unreadable)"
	}
	return strings.TrimRight(fmt.Sprintf("%s %-30s %-22s %-4s %s", arrow, intent, peer, via, status), " ")
}

// model renders a model call, in the aurora.
func (h *Handler) model(rec record) string {
	line := "✦ model "
	if a := rec.get("agent"); a != "" {
		line += a + "  "
	}
	line += fmt.Sprintf("%s  %s  %s  %s→%s tokens", rec.get("model"), rec.get("booking"), rec.get("latency"), rec.get("inputTokens"), rec.get("outputTokens"))
	if t := rec.get("tools"); t != "" && t != "[]" {
		line += "  tools " + t
	}
	if a := rec.get("attempt"); a != "" && a != "1" {
		line += "  attempt " + a
	}
	if s := rec.get("skills"); s != "" && s != "[]" {
		line += "  skills " + s
	}
	out := h.aurora(line)
	switch rec.get("outcome") {
	case "ok":
		return out + "  " + h.paint(h.p.Green, "ok", false)
	case "invalid":
		return out + "  " + h.paint(h.p.Yellow, "invalid: "+rec.get("reason"), false)
	}
	return out + "  " + h.paint(h.p.Red, rec.get("outcome")+": "+rec.get("reason"), false)
}
