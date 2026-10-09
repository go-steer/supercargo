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
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/go-steer/supercargo/internal/assistant"
	"github.com/go-steer/supercargo/internal/llm"
	"github.com/go-steer/supercargo/internal/llm/fake"
	"github.com/go-steer/supercargo/internal/shipper"
	"github.com/go-steer/supercargo/pkg/intents"
)

// bookingNode confirms every booking, and records what it was sent.
type bookingNode struct {
	node
	mu     sync.Mutex
	booked []shipper.BookRequest
	err    error
}

func (n *bookingNode) Book(_ context.Context, r shipper.BookRequest) (*shipper.BookResponse, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.err != nil {
		return nil, n.err
	}
	n.booked = append(n.booked, r)
	return &shipper.BookResponse{Confirmation: &intents.BookingConfirmationBody{BookingRef: "OB-BKG-1042", Status: intents.StatusConfirmed,
		EBL: &intents.EBL{TransportDocumentReference: "OB-TD-1", VesselName: "Ocean Blue Aurora", VoyageNumber: "42W", PortOfLoading: "NLRTM", PortOfDischarge: "USEWR", ShipperName: "Cymbal Shoes", ConsigneeName: "To order", Status: "BOOKED", Containers: []intents.ShippingContainer{{ContainerNumber: "OCBU0000004", EquipmentSizeType: "45G1", SealNumber: "S1", GrossWeightKg: 12000}}}, PlannedDeparture: "2026-10-13T18:00:00Z", PlannedArrival: "2026-10-14T08:00:00Z"}}, nil
}

func (n *bookingNode) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.booked)
}

// clock is a settable clock.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func dialogModel(t *testing.T, m llm.Model, n assistant.Node) (*Model, *clock) {
	t.Helper()
	c := &clock{t: noon}
	a := &assistant.Assistant{Model: m, Node: n, Drafts: &assistant.Drafts{},
		Self: assistant.Self{Domain: "cymbal-shoes.example", Name: "Cymbal Shoes", Endpoint: "https://cymbal-shoes.example/ulp"},
		Now:  func() time.Time { return noon }}
	mod := New(Options{Assistant: a, Theme: "dark", Node: "/run/cy.sock", Now: c.now})
	mod.Init()
	mod.Update(tea.WindowSizeMsg{Width: 110, Height: 34})
	return mod, c
}

// turn sends text and delivers the reply (and its tool events).
func turn(t *testing.T, m *Model, text string) {
	t.Helper()
	typeText(m, text)
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		if strings.HasPrefix(text, "/") {
			return // a command, handled here
		}
		t.Fatalf("%q wasn't sent", text)
	}
	m.Update(cmd())
	for len(m.events) > 0 {
		m.Update(<-m.events)
	}
}

func press(m *Model, s string) tea.Cmd {
	_, cmd := m.Update(tea.KeyPressMsg{Code: rune(s[0]), Text: s})
	return cmd
}

// run runs cmd and what it leads to (batches included), feeding results
// back, as the program would.
func run(m *Model, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	msg := cmd()
	switch msg := msg.(type) {
	case tea.BatchMsg:
		for _, c := range msg {
			run(m, c)
		}
	case nil:
	default:
		_, next := m.Update(msg)
		if _, ok := msg.(sentMsg); ok || isReply(msg) {
			run(m, next)
		}
	}
}

func isReply(msg tea.Msg) bool { _, ok := msg.(replyMsg); return ok }

const scene2 = "Book one 40ft high-cube of sneakers, Rotterdam to Newark, departing next week, with ocean-blue.example. " +
	"Consignee is Cymbal Footwear in Newark (cymbal-footwear.example). It has to arrive before Thursday the 15th."

// TestScene2: the model drafts; the dialog shows what the carrier will
// learn and what stays home; only a "y" in the dialog sends; the
// assistant reports the outcome.
func TestScene2(t *testing.T) {
	n := &bookingNode{}
	m, c := dialogModel(t, assistant.FakeModel(func() time.Time { return noon }), n)
	turn(t, m, scene2)
	if m.dialog == nil {
		t.Fatalf("no dialog:\n%s", screen(m))
	}
	s := screen(m)
	for _, want := range []string{"Booking draft BD-0001", "Operator:  ocean-blue.example", "Route:     NLRTM Rotterdam → USEWR Newark",
		"Equipment: 1 × 40HC (45G1), 12000 kg each", "Shipper:   Cymbal Shoes", "Cargo:     Footwear (sneakers), HS 6404", "Departure: on/after 2026-10-12",
		"Consignee: Cymbal Footwear (cymbal-footwear.example)", "Updates:   ulp.tnt.event, ulp.tnt.alert → https://cymbal-shoes.example/ulp",
		"Not sent:  your deadline, 2026-10-15 (it stays with us)", "Send this booking request? [y/N]"} {
		if !strings.Contains(s, want) {
			t.Errorf("the dialog lacks %q:\n%s", want, s)
		}
	}
	// A "y" the moment the dialog opens isn't an answer to it.
	if cmd := press(m, "y"); cmd != nil || m.dialog == nil || n.count() != 0 {
		t.Fatal("an immediate y answered the dialog")
	}
	c.t = c.t.Add(time.Second)
	run(m, press(m, "y"))
	if n.count() != 1 {
		t.Fatalf("%d bookings sent", n.count())
	}
	sent, _ := json.Marshal(n.booked[0])
	if strings.Contains(string(sent), "2026-10-15") || n.booked[0].Operator != "ocean-blue.example" {
		t.Errorf("sent %s", sent)
	}
	s = screen(m)
	for _, want := range []string{"Sending BD-0001 to ocean-blue.example", "Sent BD-0001 to ocean-blue.example, signed. Answer: confirmed OB-BKG-1042.",
		"Booked. Reference OB-BKG-1042 on vessel Ocean Blue Aurora, voyage 42W", "transport document OB-TD-1",
		"the day before your deadline of" /* the date wraps at this width; TestReport checks it */} {
		if !strings.Contains(s, want) {
			t.Errorf("the screen lacks %q:\n%s", want, s)
		}
	}
	if m.dialog != nil || m.busy || !m.input.Focused() {
		t.Error("the dialog didn't close cleanly")
	}
	// The draft is gone: it can't be sent twice.
	turn(t, m, "/send BD-0001")
	if m.dialog != nil || !strings.Contains(screen(m), "No open draft BD-0001") {
		t.Error("a sent draft reopened")
	}
}

// TestModelCantSend: whatever the model says, nothing is sent without
// the person's key in the open dialog.
func TestModelCantSend(t *testing.T) {
	n := &bookingNode{}
	draft := llm.GenerateResponse{ToolCalls: []llm.ToolCall{fake.Call("1", assistant.ToolDraftBooking,
		`{"operator":"ocean-blue.example","portOfLoading":"NLRTM","portOfDischarge":"USEWR","grossWeightKg":12000,`+
			`"containerType":"40HC","containerCount":1,"cargoDescription":"Shoes","hsCode":"6404","earliestDeparture":"2026-10-12"}`)}}
	m, c := dialogModel(t, fake.Turns(draft, llm.GenerateResponse{Text: "y"}, llm.GenerateResponse{Text: "Sent! (y)"}), n)
	turn(t, m, "Book with ocean-blue.example")
	c.t = c.t.Add(time.Second)
	if m.dialog == nil || n.count() != 0 {
		t.Fatalf("dialog %v, %d sent", m.dialog, n.count())
	}
	// Declining keeps the draft; /send reopens it; another key declines.
	press(m, "n")
	if m.dialog != nil || n.count() != 0 || !strings.Contains(screen(m), "Not sent. Draft BD-0001 stays open") {
		t.Fatalf("decline:\n%s", screen(m))
	}
	turn(t, m, "/send BD-0001")
	if m.dialog == nil {
		t.Fatal("/send didn't reopen the dialog")
	}
	c.t = c.t.Add(time.Second)
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.dialog != nil || n.count() != 0 {
		t.Error("enter sent")
	}
	turn(t, m, "/send nope")
	if !strings.Contains(screen(m), "No open draft nope") {
		t.Error("an unknown draft")
	}
}

func TestSendFailures(t *testing.T) {
	n := &bookingNode{err: &assistant.NodeError{Status: 422, Message: "not sent: it would carry private text"}}
	m, c := dialogModel(t, assistant.FakeModel(func() time.Time { return noon }), n)
	turn(t, m, scene2)
	c.t = c.t.Add(time.Second)
	run(m, press(m, "y"))
	if !strings.Contains(screen(m), "Draft BD-0001: not sent: it would carry private text") || m.busy {
		t.Errorf("a refused send:\n%s", screen(m))
	}
	// Refused before sending: the draft is back, and can be sent again.
	n.err = nil
	turn(t, m, "/send BD-0001")
	c.t = c.t.Add(time.Second)
	run(m, press(m, "y"))
	if n.count() != 1 {
		t.Errorf("%d sent after the retry", n.count())
	}
}

// TestDialogKeys: early keys, held keys, pastes and keys during a send
// are ignored; a second draft waits for the first one's answer.
func TestDialogKeys(t *testing.T) {
	n := &bookingNode{}
	two := llm.GenerateResponse{ToolCalls: []llm.ToolCall{
		fake.Call("1", assistant.ToolDraftBooking, draftInput("Shoes")), fake.Call("2", assistant.ToolDraftBooking, draftInput("Boots"))}}
	m, c := dialogModel(t, fake.Turns(two, llm.GenerateResponse{Text: "Two drafts."}, llm.GenerateResponse{Text: "Done."}), n)
	turn(t, m, "Book twice with ocean-blue.example")
	// Within the arm delay, even a decline is ignored.
	press(m, "n")
	if m.dialog == nil || m.dialog.DraftID != "BD-0001" {
		t.Fatal("an early key answered the dialog")
	}
	c.t = c.t.Add(time.Second)
	m.Update(tea.PasteMsg{Content: "y"})
	if _, cmd := m.Update(tea.KeyPressMsg{Code: 'y', Text: "y", IsRepeat: true}); cmd != nil || n.count() != 0 {
		t.Fatal("a paste or a held key sent")
	}
	cmd := press(m, "y")
	// While BD-0001 is in flight, BD-0002 waits.
	if m.dialog != nil || !m.busy {
		t.Fatalf("dialog %v during the send", m.dialog)
	}
	run(m, cmd)
	if m.dialog == nil || m.dialog.DraftID != "BD-0002" || n.count() != 1 {
		t.Fatalf("after the first send: dialog %v, %d sent", m.dialog, n.count())
	}
	m.dialogAt = c.t
	c.t = c.t.Add(time.Second)
	run(m, press(m, "y"))
	if n.count() != 2 || !strings.Contains(n.booked[1].Request.Cargo.Description, "Boots") {
		t.Errorf("%d sent", n.count())
	}
}

func draftInput(cargo string) string {
	return `{"operator":"ocean-blue.example","portOfLoading":"NLRTM","portOfDischarge":"USEWR","grossWeightKg":12000,` +
		`"containerType":"40HC","containerCount":1,"cargoDescription":"` + cargo + `","hsCode":"6404","earliestDeparture":"2026-10-12"}`
}

// TestDialogShowsEverything: a long field is wrapped, not cut.
func TestDialogShowsEverything(t *testing.T) {
	long := strings.Repeat("sneakers ", 30) + "AND-THE-TAIL"
	m, _ := dialogModel(t, fake.Turns(llm.GenerateResponse{ToolCalls: []llm.ToolCall{fake.Call("1", assistant.ToolDraftBooking, draftInput(long))}},
		llm.GenerateResponse{Text: "Ready."}), &bookingNode{})
	turn(t, m, "Book with ocean-blue.example")
	if s := screen(m); !strings.Contains(s, "AND-THE-TAIL") {
		t.Errorf("the tail of a long field is hidden:\n%s", s)
	}
	// A carrier we've never booked with, not in an address book: said.
	if s := screen(m); !strings.Contains(s, "A new partner") {
		t.Errorf("no new-partner warning:\n%s", s)
	}
}

// TestUnknownOutcome: a send whose outcome is unknown isn't offered
// again, and the assistant is told.
func TestUnknownOutcome(t *testing.T) {
	n := &bookingNode{err: errors.New("the node: EOF")}
	m, c := dialogModel(t, assistant.FakeModel(func() time.Time { return noon }), n)
	turn(t, m, scene2)
	c.t = c.t.Add(time.Second)
	run(m, press(m, "y"))
	if !strings.Contains(screen(m), "may or may not have been sent") || m.busy || m.dialog != nil {
		t.Errorf("unknown outcome:\n%s", screen(m))
	}
	turn(t, m, "/send BD-0001")
	if m.dialog != nil {
		t.Error("an unknown-outcome draft was offered again")
	}
}
