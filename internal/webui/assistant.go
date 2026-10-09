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

package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-steer/supercargo/internal/activity"
	"github.com/go-steer/supercargo/internal/assistant"
	"github.com/go-steer/supercargo/internal/control"
)

// The browser's assistant is the shipper role's assistant (the same Go
// code ulpchat runs), one per session, with its own conversation and
// drafts. As in ulpchat, the model drafts and nothing else: a booking is
// sent only by the send route, which only a person's click in the
// confirmation dialog calls, and only once the node handed the draft to
// the page ArmDelay ago.

// ArmDelay is how long a draft must have been on the page before it can
// be sent: counted from the moment the node first returned it, so a click
// (or a script) can't confirm a dialog that was never on screen. The
// page's button arms after the same delay.
var ArmDelay = 1500 * time.Millisecond

// Limits: one turn's time (model calls and tools included), how many
// turns run at once across sessions (each is model calls), and how much
// transcript a session keeps.
const (
	TurnTimeout     = 55 * time.Second
	MaxTurnsAtOnce  = 8
	MaxTranscript   = 200       // lines
	MaxTranscriptKB = 256 << 10 // bytes of text
)

// turnSlots bounds the turns running at once, across every session.
var turnSlots = make(chan struct{}, MaxTurnsAtOnce)

// Line is one line of the transcript: a person's message, the assistant's
// answer (with the tools it called and the drafts it made), or a note
// about a send.
type Line struct {
	Who    string                  `json:"who"` // "you", "assistant" or "note"
	Text   string                  `json:"text"`
	Time   time.Time               `json:"time"`
	Tools  []ToolLine              `json:"tools,omitempty"`
	Drafts []assistant.DraftResult `json:"drafts,omitempty"`
}

// ToolLine is a tool call as the transcript shows it. Detail is a short
// summary of the model's arguments (one line, capped): text for the page
// to show as text, never markup.
type ToolLine struct {
	Name   string `json:"name"`
	Detail string `json:"detail,omitempty"`
	Error  string `json:"error,omitempty"`
}

// toolDetail summarizes a call's arguments: the values of its top-level
// members, in order, as one line of at most 80 characters.
func toolDetail(input json.RawMessage) string {
	dec := json.NewDecoder(bytes.NewReader(input))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return ""
	}
	var parts []string
	for dec.More() {
		if _, err := dec.Token(); err != nil { // the member's name
			break
		}
		var v any
		if err := dec.Decode(&v); err != nil {
			break
		}
		switch v := v.(type) {
		case string:
			parts = append(parts, v)
		case float64, bool:
			parts = append(parts, fmt.Sprint(v))
		}
	}
	return assistant.OneLine(strings.Join(parts, ", "), 80)
}

// Progress is the work in progress in this session: the tool calls made
// so far, and the one running, if any. Between them the model is thinking.
type Progress struct {
	Working bool       `json:"working"`
	Tools   []ToolLine `json:"tools"`
	Running *ToolLine  `json:"running,omitempty"`
}

type chat struct {
	a    *assistant.Assistant
	conv *assistant.Conversation
	// work serialises what drives the model in this session: a turn, or a
	// send and its report. A second tab asking meanwhile is told it's busy.
	work sync.Mutex

	mu      sync.Mutex
	lines   []Line
	tools   []ToolLine                       // the turn in progress
	busy    bool                             // a turn, or a send and its report, is running
	running *ToolLine                        // the tool call running now
	drafts  map[string]assistant.DraftResult // open: drafted, not sent or discarded
	order   []string                         // open drafts, in the order made
	shown   map[string]time.Time             // when the node first handed each draft to the page
}

func (c *chat) add(l Line) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, l)
	size := 0
	for _, x := range c.lines {
		size += len(x.Text)
	}
	for len(c.lines) > 1 && (len(c.lines) > MaxTranscript || size > MaxTranscriptKB) {
		size -= len(c.lines[0].Text)
		c.lines = c.lines[1:]
	}
}

func (c *chat) open(drafts []assistant.DraftResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, d := range drafts {
		if _, ok := c.drafts[d.DraftID]; !ok {
			c.order = append(c.order, d.DraftID)
		}
		c.drafts[d.DraftID] = d
	}
}

func (c *chat) close(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.drafts, id)
	delete(c.shown, id)
	c.order = slices.DeleteFunc(c.order, func(o string) bool { return o == id })
}

// AssistantResponse is the assistant's state for this session.
type AssistantResponse struct {
	Available bool                    `json:"available"`
	Lines     []Line                  `json:"lines"`
	Drafts    []assistant.DraftResult `json:"drafts"`
	Error     string                  `json:"error,omitempty"`
}

// state is what the page sees; handing it the open drafts starts their
// ArmDelay, once.
func (c *chat) state(now time.Time) AssistantResponse {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := AssistantResponse{Available: true, Lines: slices.Clone(c.lines), Drafts: []assistant.DraftResult{}}
	for _, id := range c.order {
		r.Drafts = append(r.Drafts, c.drafts[id])
		if _, ok := c.shown[id]; !ok {
			c.shown[id] = now
		}
	}
	return r
}

// chatFor is the session's assistant, made on first use.
func (s *server) chatFor(r *http.Request) (*chat, error) {
	if s.cfg.NewAssistant == nil {
		return nil, errors.New("this node has no assistant")
	}
	id := s.sessionID(r)
	s.mu.Lock()
	sess, ok := s.lookup(id)
	var c *chat
	var persona string
	if ok {
		c, persona = sess.chat, sess.persona.ID
	}
	s.mu.Unlock()
	if !ok {
		return nil, errors.New("sign in first")
	}
	if c != nil {
		return c, nil
	}
	// Made outside the lock: it reads the skills and business files.
	a, err := s.cfg.NewAssistant()
	if err != nil {
		return nil, err
	}
	c = &chat{a: a, conv: &assistant.Conversation{}, drafts: map[string]assistant.DraftResult{}, shown: map[string]time.Time{}}
	// The node's activity says whose assistant called a model.
	if report := a.OnModelCall; report != nil {
		who := "assistant · " + persona
		a.OnModelCall = func(m activity.Model) { m.Agent = who; report(m) }
	}
	a.OnToolStart = func(ev assistant.ToolEvent) {
		c.mu.Lock()
		c.running = &ToolLine{Name: ev.Name, Detail: toolDetail(ev.Input)}
		c.mu.Unlock()
	}
	a.OnTool = func(ev assistant.ToolEvent) {
		c.mu.Lock()
		c.tools = append(c.tools, ToolLine{Name: ev.Name, Detail: toolDetail(ev.Input), Error: assistant.OneLine(ev.Error, 200)})
		c.running = nil
		c.mu.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok = s.lookup(id)
	if !ok {
		return nil, errors.New("sign in first")
	}
	if sess.chat == nil { // another request may have made one meanwhile
		sess.chat = c
	}
	return sess.chat, nil
}

func (s *server) assistantState(w http.ResponseWriter, r *http.Request, _ Persona) {
	if s.cfg.NewAssistant == nil {
		control.Answer(w, http.StatusOK, AssistantResponse{Lines: []Line{}, Drafts: []assistant.DraftResult{}})
		return
	}
	c, err := s.chatFor(r)
	if err != nil {
		control.Answer(w, http.StatusInternalServerError, AssistantResponse{Error: err.Error()})
		return
	}
	control.Answer(w, http.StatusOK, c.state(s.cfg.Now()))
}

// assistantProgress is what the page shows while a turn runs. It hands
// out no drafts, so polling it never starts a draft's ArmDelay.
func (s *server) assistantProgress(w http.ResponseWriter, r *http.Request, _ Persona) {
	if s.cfg.NewAssistant == nil {
		control.Answer(w, http.StatusOK, Progress{Tools: []ToolLine{}})
		return
	}
	c, err := s.chatFor(r)
	if err != nil {
		control.Answer(w, http.StatusInternalServerError, AssistantResponse{Error: err.Error()})
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	p := Progress{Working: c.busy, Tools: []ToolLine{}}
	if c.busy {
		p.Tools = slices.Clone(c.tools)
		if c.running != nil {
			run := *c.running
			p.Running = &run
		}
	}
	control.Answer(w, http.StatusOK, p)
}

// begin takes the session's work lock and a turn slot, or answers why not.
func (s *server) begin(w http.ResponseWriter, c *chat) bool {
	if !c.work.TryLock() {
		control.Answer(w, http.StatusConflict, AssistantResponse{Error: "the assistant is busy with this session's previous request"})
		return false
	}
	select {
	case turnSlots <- struct{}{}:
		c.mu.Lock()
		c.busy, c.tools, c.running = true, nil, nil
		c.mu.Unlock()
		return true
	default:
		c.work.Unlock()
		control.Answer(w, http.StatusServiceUnavailable, AssistantResponse{Error: "the assistant is busy; try again in a moment"})
		return false
	}
}

func (s *server) end(c *chat) {
	c.mu.Lock()
	c.busy, c.running = false, nil
	c.mu.Unlock()
	<-turnSlots
	c.work.Unlock()
}

func (s *server) assistantTurn(w http.ResponseWriter, r *http.Request, _ Persona) {
	var req struct {
		Text string `json:"text"`
	}
	c, ok := s.chatRequest(w, r, &req)
	if !ok || !s.begin(w, c) {
		return
	}
	defer s.end(c)
	now := s.cfg.Now()
	ctx, cancel := context.WithTimeout(r.Context(), TurnTimeout)
	defer cancel()
	c.mu.Lock()
	c.tools = nil
	c.mu.Unlock()
	reply, err := c.a.Turn(ctx, c.conv, req.Text)
	if errors.Is(err, assistant.ErrTooLong) || errors.Is(err, assistant.ErrBusy) {
		code := http.StatusBadRequest
		if errors.Is(err, assistant.ErrBusy) {
			code = http.StatusConflict
		}
		control.Answer(w, code, AssistantResponse{Error: err.Error()})
		return
	}
	c.add(Line{Who: "you", Text: req.Text, Time: now})
	c.mu.Lock()
	tools := c.tools
	c.mu.Unlock()
	// Drafts made before a failure are real (and sendable): keep them.
	c.open(reply.Drafts)
	if err != nil {
		c.add(Line{Who: "note", Text: "The assistant couldn't finish answering: " + assistant.OneLine(err.Error(), 200), Time: s.cfg.Now(),
			Tools: tools, Drafts: reply.Drafts})
		st := c.state(s.cfg.Now())
		st.Error = "the assistant couldn't answer"
		control.Answer(w, http.StatusBadGateway, st)
		return
	}
	c.add(Line{Who: "assistant", Text: reply.Text, Time: s.cfg.Now(), Tools: tools, Drafts: reply.Drafts})
	control.Answer(w, http.StatusOK, c.state(s.cfg.Now()))
}

// SendRequest confirms or discards a draft.
type SendRequest struct {
	DraftID string `json:"draftId"`
}

// assistantSend sends a draft a person confirmed in the dialog, then has
// the assistant report the outcome.
func (s *server) assistantSend(w http.ResponseWriter, r *http.Request, _ Persona) {
	var req SendRequest
	c, ok := s.chatRequest(w, r, &req)
	if !ok {
		return
	}
	c.mu.Lock()
	shown, wasShown := c.shown[req.DraftID]
	_, open := c.drafts[req.DraftID]
	c.mu.Unlock()
	if !open {
		control.Answer(w, http.StatusNotFound, AssistantResponse{Error: "no open draft " + assistant.OneLine(req.DraftID, 32)})
		return
	}
	if !wasShown || s.cfg.Now().Sub(shown) < ArmDelay {
		control.Answer(w, http.StatusTooEarly, AssistantResponse{Error: "the dialog has to be on screen a moment before it can be confirmed"})
		return
	}
	if !s.begin(w, c) {
		return
	}
	defer s.end(c)
	// A send the person confirmed finishes even if the tab closes: cutting
	// it off would leave its outcome unknown.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), TurnTimeout)
	defer cancel()
	sent, err := c.a.Send(ctx, req.DraftID)
	switch {
	case errors.Is(err, assistant.ErrNoDraft):
		// Sent or discarded by another request just now: say nothing more.
		c.close(req.DraftID)
		control.Answer(w, http.StatusNotFound, AssistantResponse{Error: "no open draft " + assistant.OneLine(req.DraftID, 32)})
		return
	case err != nil:
		if _, still := c.a.Drafts.Get(req.DraftID); still != nil {
			c.close(req.DraftID) // gone: it can't be sent twice
		}
		if errors.Is(err, assistant.ErrOutcomeUnknown) {
			// As in ulpchat: the model must not think the draft is unsent.
			_ = c.conv.Note("Sending draft " + req.DraftID + " failed with an unknown outcome: it may have reached the carrier. " +
				"The draft is gone; tell the user to check the shipments before booking again.")
		}
		c.add(Line{Who: "note", Text: "Draft " + req.DraftID + ": " + assistant.OneLine(err.Error(), 300), Time: s.cfg.Now()})
		st := c.state(s.cfg.Now())
		st.Error = assistant.OneLine(err.Error(), 300)
		control.Answer(w, http.StatusBadGateway, st)
		return
	}
	c.close(req.DraftID)
	status, ref := "", ""
	if sent.Confirmation != nil {
		status, ref = sent.Confirmation.Status, sent.Confirmation.BookingRef
	}
	c.add(Line{Who: "note", Text: strings.TrimSpace("Sent " + req.DraftID + " to " + sent.Draft.Operator + ", signed. Answer: " + status + " " + ref),
		Time: s.cfg.Now()})
	c.mu.Lock()
	c.tools = nil
	c.mu.Unlock()
	if reply, err := c.a.Report(ctx, c.conv, sent); err == nil {
		c.add(Line{Who: "assistant", Text: reply.Text, Time: s.cfg.Now()})
	} else {
		_ = c.conv.Note("Draft " + req.DraftID + " was sent: " + status + " " + ref + ".")
	}
	control.Answer(w, http.StatusOK, c.state(s.cfg.Now()))
}

func (s *server) assistantDiscard(w http.ResponseWriter, r *http.Request, _ Persona) {
	var req SendRequest
	c, ok := s.chatRequest(w, r, &req)
	if !ok {
		return
	}
	if _, err := c.a.Drafts.Take(req.DraftID); err != nil {
		control.Answer(w, http.StatusNotFound, AssistantResponse{Error: "no open draft " + assistant.OneLine(req.DraftID, 32)})
		return
	}
	c.close(req.DraftID)
	c.add(Line{Who: "note", Text: "Draft " + req.DraftID + " discarded; nothing was sent.", Time: s.cfg.Now()})
	control.Answer(w, http.StatusOK, c.state(s.cfg.Now()))
}

// chatRequest checks a state-changing assistant request (same origin,
// JSON) and decodes it into v.
func (s *server) chatRequest(w http.ResponseWriter, r *http.Request, v any) (*chat, bool) {
	if !s.sameOrigin(r) {
		control.Answer(w, http.StatusForbidden, AssistantResponse{Error: "cross-site request refused"})
		return nil, false
	}
	if err := control.Decode(r, v); err != nil {
		control.Answer(w, http.StatusBadRequest, AssistantResponse{Error: err.Error()})
		return nil, false
	}
	c, err := s.chatFor(r)
	if err != nil {
		control.Answer(w, http.StatusConflict, AssistantResponse{Error: err.Error()})
		return nil, false
	}
	return c, true
}

// disrupt forwards a carrier's "disrupt voyage" (confirmed in its dialog)
// to the control API: the same action as ulpctl disrupt, validated there.
func (s *server) disrupt(w http.ResponseWriter, r *http.Request, _ Persona) {
	if !s.sameOrigin(r) {
		control.Answer(w, http.StatusForbidden, errorBody("cross-site request refused"))
		return
	}
	var req struct {
		BookingRef string `json:"bookingRef"`
		Delay      string `json:"delay"`
		Reason     string `json:"reason"`
	}
	if err := control.Decode(r, &req); err != nil {
		control.Answer(w, http.StatusBadRequest, errorBody(err.Error()))
		return
	}
	b, _ := json.Marshal(req) // three strings: always marshals
	// A constant method and path: the request always builds.
	r2, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, "/disrupt", bytes.NewReader(b))
	r2.Header.Set("Content-Type", "application/json")
	s.cfg.Control.ServeHTTP(w, r2)
}
