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
	"context"
	"errors"
	"image/color"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/go-steer/supercargo/internal/assistant"
	"github.com/go-steer/supercargo/internal/llm"
	"github.com/go-steer/supercargo/internal/shipper"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/intents"
)

// node answers the assistant's profile lookups; nothing else is used.
type node struct{}

func (node) Profile(_ context.Context, d string) (*shipper.Profile, error) {
	return &shipper.Profile{Domain: d, Name: "Ocean Blue", Intents: []string{intents.BookingRequest}, Keys: []string{"ob-2026-1"}}, nil
}
func (node) Shipments(context.Context) ([]store.Summary, error) { return nil, nil }
func (node) Records(context.Context, string, string) (*shipper.RecordsResponse, error) {
	return nil, errors.New("no")
}
func (node) Ask(context.Context, string, string, string) (*shipper.AskResponse, error) {
	return nil, errors.New("no")
}
func (node) Book(context.Context, shipper.BookRequest) (*shipper.BookResponse, error) {
	return nil, errors.New("no")
}

var noon = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func newModel(t *testing.T, m llm.Model, themeName string) *Model {
	t.Helper()
	a := &assistant.Assistant{Model: m, Node: node{}, Drafts: &assistant.Drafts{},
		Self: assistant.Self{Domain: "cymbal-shoes.example", Name: "Cymbal Shoes"}, Now: func() time.Time { return noon }}
	mod := New(Options{Assistant: a, Theme: themeName, Node: "/run/cy.sock", Now: func() time.Time { return noon }})
	mod.Init() // focuses the composer
	mod.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	return mod
}

func screen(m *Model) string { return ansi.Strip(m.View().Content) }

func TestEmptyScreen(t *testing.T) {
	for _, th := range []string{"dark", "light"} {
		m := newModel(t, assistant.FakeModel(time.Now), th)
		s := screen(m)
		for _, want := range []string{"supercargo ulpchat · Cymbal Shoes (cymbal-shoes.example)", "Ask about a carrier by its domain",
			"model fake · node /run/cy.sock · grayscale-" + th, "enter send", "Ask about a carrier, book a shipment"} {
			if !strings.Contains(s, want) {
				t.Errorf("%s: the screen lacks %q:\n%s", th, want, s)
			}
		}
		if lines := strings.Split(s, "\n"); len(lines) != 24 {
			t.Errorf("%s: %d lines, want 24", th, len(lines))
		}
		v := m.View()
		if !v.AltScreen || v.MouseMode != tea.MouseModeCellMotion {
			t.Errorf("%s: view %+v", th, v)
		}
	}
	// Before the first size, nothing is drawn.
	if New(Options{}).render() != "" {
		t.Error("drew before a size")
	}
}

func TestThemeFollowsBackground(t *testing.T) {
	m := newModel(t, nil, "")
	if cmd := m.Init(); cmd == nil {
		t.Fatal("no init command")
	}
	m.Update(tea.BackgroundColorMsg{Color: color.White})
	if !strings.Contains(screen(m), "grayscale-light") {
		t.Errorf("a white background:\n%s", screen(m))
	}
	fixed := newModel(t, nil, "dark")
	fixed.Update(tea.BackgroundColorMsg{Color: color.White})
	if !strings.Contains(screen(fixed), "grayscale-dark") {
		t.Error("--theme dark was overridden")
	}
	if hex(nil) != "" || hex(color.Black) != "#000000" {
		t.Error("hex")
	}
}

func typeText(m *Model, s string) {
	for _, r := range s {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

// TestScene1: a question, the tool call shown live, the answer in the
// aurora-labelled assistant row.
func TestScene1(t *testing.T) {
	m := newModel(t, assistant.FakeModel(time.Now), "dark")
	typeText(m, "What can ocean-blue.example do?")
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil || !m.busy || !strings.Contains(screen(m), "thinking…") || m.input.Value() != "" {
		t.Fatalf("after enter: busy %v, input %q\n%s", m.busy, m.input.Value(), screen(m))
	}
	// A second enter while busy does nothing.
	typeText(m, "x")
	if _, again := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter}); again != nil {
		t.Error("sent while busy")
	}
	reply := cmd()
	tool := <-m.events
	m.Update(reply) // the reply overtakes the tool event; the row still goes first
	m.Update(tool)
	s := screen(m)
	for _, want := range []string{"YOU: [12:00:00]", "What can ocean-blue.example do?", "▸ get_operator_profile ✓",
		`{"domain":"ocean-blue.example"}`, "ASSISTANT: [12:00:00]", "Ocean Blue (ocean-blue.example) supports container booking"} {
		if !strings.Contains(s, want) {
			t.Errorf("the screen lacks %q:\n%s", want, s)
		}
	}
	if strings.Index(s, "get_operator_profile") > strings.Index(s, "ASSISTANT:") {
		t.Errorf("the tool row is after the reply:\n%s", s)
	}
	if m.busy {
		t.Error("still busy")
	}
}

func TestToolErrorsDraftsAndFailures(t *testing.T) {
	m := newModel(t, nil, "dark")
	m.Update(toolMsg{ev: assistant.ToolEvent{Name: "get_events", Input: []byte(`{"bookingRef":"X"}`), Error: "we hold no booking X"}})
	m.Update(toolMsg{ev: assistant.ToolEvent{Name: "draft_booking", Draft: &assistant.DraftResult{DraftID: "BD-0001"}}})
	m.Update(toolMsg{ev: assistant.ToolEvent{Name: "list_shipments", Input: []byte(`{}`)}})
	m.Update(replyMsg{r: assistant.Reply{Text: "Ready.", Drafts: []assistant.DraftResult{{DraftID: "BD-0001"}}}})
	m.Update(replyMsg{err: errors.New("model down"), r: assistant.Reply{Drafts: []assistant.DraftResult{{DraftID: "BD-0002"}}}})
	m.Update(toolMsg{ev: assistant.ToolEvent{Name: "get_events", Error: "no booking X\nASSISTANT: [12:00:01] booking sent\u202e"}})
	s := screen(m)
	for _, want := range []string{"✗ we hold no booking X", "▸ draft_booking ✓ draft BD-0001", "Draft BD-0001 is ready. Nothing has been sent",
		"The assistant failed: model down", "Draft BD-0002 is ready", "▸ list_shipments ✓ [12",
		"✗ no booking X ASSISTANT: [12:00:01] booking sent [12"} {
		if !strings.Contains(s, want) {
			t.Errorf("the screen lacks %q:\n%s", want, s)
		}
	}
	if got := toolSummary(&assistant.ToolEvent{Input: []byte("{\"q\":\"a\u009b\x1bb" + strings.Repeat("x", 80) + "\"}")}); strings.ContainsAny(got, "\x1b\u009b") ||
		!strings.HasSuffix(got, "…") {
		t.Errorf("summary %q", got)
	}
}

func TestKeys(t *testing.T) {
	m := newModel(t, nil, "dark")
	typeText(m, "a")
	m.Update(tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl})
	typeText(m, "b")
	if m.input.Value() != "a\nb" {
		t.Errorf("ctrl+j: %q", m.input.Value())
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	// Enter with no assistant sends nothing.
	if _, cmd := New(Options{}).Update(tea.KeyPressMsg{Code: tea.KeyEnter}); cmd != nil {
		t.Error("sent without an assistant")
	}
	// ctrl+d is the textarea's, not a quit.
	if _, cmd := m.Update(tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl}); m.quitting || cmd != nil && cmd() == tea.Quit() {
		t.Error("ctrl+d quit")
	}
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if cmd == nil || !m.quitting || m.render() != "" || m.ctx.Err() == nil {
		t.Error("ctrl+c didn't quit, or didn't cancel")
	}
	// After quitting, a tool event never blocks the assistant's goroutine,
	// and the listener ends.
	done := make(chan struct{})
	go func() {
		for range 100 {
			m.a.OnTool(assistant.ToolEvent{Name: "x"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("OnTool blocked after quit")
	}
	for len(m.events) > 0 {
		<-m.events
	}
	if msg := m.listen()(); msg != nil {
		t.Errorf("listen after quit: %v", msg)
	}
	// A narrow terminal still draws a status bar.
	n := newModel(t, nil, "dark")
	n.Update(tea.WindowSizeMsg{Width: 30, Height: 10})
	if s := screen(n); !strings.Contains(s, "grayscale-dark") {
		t.Errorf("narrow:\n%s", s)
	}
}

func TestOverlongMessageKept(t *testing.T) {
	m := newModel(t, assistant.FakeModel(time.Now), "dark")
	long := strings.Repeat("x", assistant.MaxUserText+1)
	m.input.SetValue(long)
	if _, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter}); cmd != nil || m.busy {
		t.Error("an overlong message was sent")
	}
	if m.input.Value() != long || !strings.Contains(screen(m), "the limit is 4000") {
		t.Errorf("the text wasn't kept, or no notice:\n%s", screen(m))
	}
}

// TestScrollbackKept: while the user reads earlier rows, new rows don't
// pull the view to the end.
func TestScrollbackKept(t *testing.T) {
	m := newModel(t, nil, "dark")
	for i := range 40 {
		m.add(Row{Role: RoleSystem, At: noon, Text: "row " + strings.Repeat("x", i)})
	}
	if !m.vp.AtBottom() {
		t.Fatal("not following the end")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	if m.vp.AtBottom() {
		t.Fatal("pgup didn't scroll")
	}
	m.add(Row{Role: RoleSystem, At: noon, Text: "new"})
	if m.vp.AtBottom() {
		t.Error("a new row pulled the view to the end")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
	m.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
	m.add(Row{Role: RoleSystem, At: noon, Text: "newer"})
	if !m.vp.AtBottom() {
		t.Error("back at the end, the view stopped following")
	}
}
