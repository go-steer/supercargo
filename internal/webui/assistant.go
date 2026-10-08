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
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-steer/supercargo/internal/assistant"
	"github.com/go-steer/supercargo/internal/control"
)

// The browser's assistant is the shipper role's assistant (the same Go
// code ulpchat runs), one per session, with its own conversation and
// drafts. As in ulpchat, the model drafts and nothing else: a booking is
// sent only by Send, which only a person's click in the confirmation
// dialog calls, and only once the dialog has been open ArmDelay.

// ArmDelay is how long a draft must have existed before it can be sent:
// a click (or a script) can't confirm a dialog that was never on screen.
var ArmDelay = 1500 * time.Millisecond

// TurnTimeout bounds one turn, model calls and tools included.
const TurnTimeout = 55 * time.Second

// MaxTranscript is how many lines a session's transcript keeps.
const MaxTranscript = 200

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

// ToolLine is a tool call as the transcript shows it.
type ToolLine struct {
	Name  string `json:"name"`
	Error string `json:"error,omitempty"`
}

type chat struct {
	a    *assistant.Assistant
	conv *assistant.Conversation

	mu     sync.Mutex
	lines  []Line
	tools  []ToolLine                       // the turn in progress
	drafts map[string]assistant.DraftResult // open: drafted, not sent or discarded
}

func (c *chat) add(l Line) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, l)
	if len(c.lines) > MaxTranscript {
		c.lines = c.lines[len(c.lines)-MaxTranscript:]
	}
}

// AssistantResponse is the assistant's state for this session.
type AssistantResponse struct {
	Available bool                    `json:"available"`
	Lines     []Line                  `json:"lines"`
	Drafts    []assistant.DraftResult `json:"drafts"`
	Error     string                  `json:"error,omitempty"`
}

func (c *chat) state() AssistantResponse {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := AssistantResponse{Available: true, Lines: slices.Clone(c.lines), Drafts: []assistant.DraftResult{}}
	for _, d := range c.drafts {
		r.Drafts = append(r.Drafts, d)
	}
	slices.SortFunc(r.Drafts, func(a, b assistant.DraftResult) int { return strings.Compare(a.DraftID, b.DraftID) })
	return r
}

// chatFor is the session's assistant, made on first use.
func (s *server) chatFor(r *http.Request) (*chat, error) {
	if s.cfg.NewAssistant == nil {
		return nil, errors.New("this node has no assistant")
	}
	id := s.sessionID(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.lookup(id)
	if !ok {
		return nil, errors.New("sign in first")
	}
	if sess.chat == nil {
		a, err := s.cfg.NewAssistant()
		if err != nil {
			return nil, err
		}
		c := &chat{a: a, conv: &assistant.Conversation{}, drafts: map[string]assistant.DraftResult{}}
		a.OnTool = func(ev assistant.ToolEvent) {
			c.mu.Lock()
			c.tools = append(c.tools, ToolLine{Name: ev.Name, Error: assistant.OneLine(ev.Error, 200)})
			c.mu.Unlock()
		}
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
	control.Answer(w, http.StatusOK, c.state())
}

func (s *server) assistantTurn(w http.ResponseWriter, r *http.Request, _ Persona) {
	var req struct {
		Text string `json:"text"`
	}
	c, ok := s.chatRequest(w, r, &req)
	if !ok {
		return
	}
	now := s.cfg.Now()
	ctx, cancel := context.WithTimeout(r.Context(), TurnTimeout)
	defer cancel()
	c.mu.Lock()
	c.tools = nil
	c.mu.Unlock()
	reply, err := c.a.Turn(ctx, c.conv, req.Text)
	switch {
	case errors.Is(err, assistant.ErrBusy):
		control.Answer(w, http.StatusConflict, AssistantResponse{Error: err.Error()})
		return
	case errors.Is(err, assistant.ErrTooLong):
		control.Answer(w, http.StatusBadRequest, AssistantResponse{Error: err.Error()})
		return
	case err != nil:
		control.Answer(w, http.StatusBadGateway, AssistantResponse{Error: "the assistant couldn't answer: " + assistant.OneLine(err.Error(), 200)})
		return
	}
	c.add(Line{Who: "you", Text: req.Text, Time: now})
	c.mu.Lock()
	tools := c.tools
	for _, d := range reply.Drafts {
		c.drafts[d.DraftID] = d
	}
	c.mu.Unlock()
	c.add(Line{Who: "assistant", Text: reply.Text, Time: s.cfg.Now(), Tools: tools, Drafts: reply.Drafts})
	control.Answer(w, http.StatusOK, c.state())
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
	d, err := c.a.Drafts.Get(req.DraftID)
	if err != nil {
		control.Answer(w, http.StatusNotFound, AssistantResponse{Error: "no open draft " + assistant.OneLine(req.DraftID, 32)})
		return
	}
	if s.cfg.Now().Sub(d.Created) < ArmDelay {
		control.Answer(w, http.StatusTooEarly, AssistantResponse{Error: "the dialog has to be on screen a moment before it can be confirmed"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), TurnTimeout)
	defer cancel()
	sent, err := c.a.Send(ctx, req.DraftID)
	if err != nil {
		c.mu.Lock()
		if _, still := c.a.Drafts.Get(req.DraftID); still != nil {
			delete(c.drafts, req.DraftID) // gone: sent or not, it can't be sent twice
		}
		c.mu.Unlock()
		c.add(Line{Who: "note", Text: "Draft " + req.DraftID + " was not sent: " + assistant.OneLine(err.Error(), 300), Time: s.cfg.Now()})
		control.Answer(w, http.StatusBadGateway, mergeError(c.state(), err))
		return
	}
	c.mu.Lock()
	delete(c.drafts, req.DraftID)
	c.mu.Unlock()
	status := ""
	if sent.Confirmation != nil {
		status = sent.Confirmation.Status
	}
	c.add(Line{Who: "note", Text: "Sent " + req.DraftID + " to " + sent.Draft.Operator + ", signed. Answer: " + status, Time: s.cfg.Now()})
	if reply, err := c.a.Report(ctx, c.conv, sent); err == nil {
		c.add(Line{Who: "assistant", Text: reply.Text, Time: s.cfg.Now()})
	}
	control.Answer(w, http.StatusOK, c.state())
}

func mergeError(st AssistantResponse, err error) AssistantResponse {
	st.Error = assistant.OneLine(err.Error(), 300)
	return st
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
	c.mu.Lock()
	delete(c.drafts, req.DraftID)
	c.mu.Unlock()
	c.add(Line{Who: "note", Text: "Draft " + req.DraftID + " discarded; nothing was sent.", Time: s.cfg.Now()})
	control.Answer(w, http.StatusOK, c.state())
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
// to the control API: the same action as ulpctl disrupt.
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
