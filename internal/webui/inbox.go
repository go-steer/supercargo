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
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-steer/supercargo/internal/assistant"
	"github.com/go-steer/supercargo/internal/control"
)

// The inbox: drafts the company's outside agents made over MCP
// (internal/mcpserver). Nothing an outside agent does sends anything; a
// person confirms each draft here, in the same dialog as the assistant's,
// behind the same arming delay, counted from when that person's dialog
// fetched that draft (GET /ui/api/inbox/draft), not from when any page
// listed the inbox.

// InboxResponse is the inbox: the open drafts, oldest first.
type InboxResponse struct {
	Drafts []assistant.DraftResult `json:"drafts"`
	// Note is the outcome of a send, for the page to show.
	Note  string `json:"note,omitempty"`
	Error string `json:"error,omitempty"`
}

func (s *server) inboxState() InboxResponse {
	r := InboxResponse{Drafts: []assistant.DraftResult{}}
	for _, d := range s.cfg.Inbox.Drafts.Open() {
		r.Drafts = append(r.Drafts, d.Result())
	}
	return r
}

// shownKey is a draft as shown to one session.
func shownKey(session, id string) string { return session + "\x00" + id }

// inboxDraft hands one draft to this session's dialog, and starts its
// arming delay for this session (once).
func (s *server) inboxDraft(w http.ResponseWriter, r *http.Request, _ Persona) {
	if s.cfg.Inbox == nil {
		control.Answer(w, http.StatusNotFound, InboxResponse{Error: "this node has no MCP inbox"})
		return
	}
	id := r.URL.Query().Get("id")
	d, err := s.cfg.Inbox.Drafts.Get(id)
	if err != nil {
		control.Answer(w, http.StatusNotFound, InboxResponse{Error: "no open draft " + assistant.OneLine(id, 32)})
		return
	}
	key := shownKey(s.sessionID(r), id)
	s.mu.Lock()
	if s.inboxShown == nil {
		s.inboxShown = map[string]time.Time{}
	}
	if _, ok := s.inboxShown[key]; !ok {
		s.inboxShown[key] = s.cfg.Now()
	}
	s.mu.Unlock()
	control.Answer(w, http.StatusOK, InboxResponse{Drafts: []assistant.DraftResult{d.Result()}})
}

// forgetShown drops every session's shown time for draft id (sent,
// discarded, or its outcome unknown).
func (s *server) forgetShown(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.inboxShown {
		if strings.HasSuffix(k, "\x00"+id) {
			delete(s.inboxShown, k)
		}
	}
}

func (s *server) inbox(w http.ResponseWriter, _ *http.Request, _ Persona) {
	if s.cfg.Inbox == nil {
		control.Answer(w, http.StatusOK, InboxResponse{Drafts: []assistant.DraftResult{}})
		return
	}
	control.Answer(w, http.StatusOK, s.inboxState())
}

// inboxRequest checks a send or a discard of an inbox draft and returns
// its ID.
func (s *server) inboxRequest(w http.ResponseWriter, r *http.Request) (string, bool) {
	if !s.sameOrigin(r) {
		control.Answer(w, http.StatusForbidden, InboxResponse{Error: "cross-site request refused"})
		return "", false
	}
	if s.cfg.Inbox == nil {
		control.Answer(w, http.StatusConflict, InboxResponse{Error: "this node has no MCP inbox"})
		return "", false
	}
	var req SendRequest
	if err := control.Decode(r, &req); err != nil {
		control.Answer(w, http.StatusBadRequest, InboxResponse{Error: err.Error()})
		return "", false
	}
	if _, err := s.cfg.Inbox.Drafts.Get(req.DraftID); err != nil {
		control.Answer(w, http.StatusNotFound, InboxResponse{Error: "no open draft " + assistant.OneLine(req.DraftID, 32)})
		return "", false
	}
	return req.DraftID, true
}

func (s *server) inboxSend(w http.ResponseWriter, r *http.Request, _ Persona) {
	id, ok := s.inboxRequest(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	shown, wasShown := s.inboxShown[shownKey(s.sessionID(r), id)]
	s.mu.Unlock()
	if !wasShown || s.cfg.Now().Sub(shown) < ArmDelay {
		control.Answer(w, http.StatusTooEarly, InboxResponse{Error: "the dialog has to be on screen a moment before it can be confirmed"})
		return
	}
	// A send the person confirmed finishes even if the tab closes.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), TurnTimeout)
	defer cancel()
	sent, err := s.cfg.Inbox.Send(ctx, id)
	if _, still := s.cfg.Inbox.Drafts.Get(id); still != nil {
		s.forgetShown(id) // gone: sent, or its outcome unknown
	}
	switch {
	case errors.Is(err, assistant.ErrNoDraft):
		control.Answer(w, http.StatusNotFound, InboxResponse{Error: "no open draft " + assistant.OneLine(id, 32)})
		return
	case err != nil:
		st := s.inboxState()
		st.Error = assistant.OneLine(err.Error(), 300)
		control.Answer(w, http.StatusBadGateway, st)
		return
	}
	st := s.inboxState()
	st.Note = inboxOutcome(sent)
	control.Answer(w, http.StatusOK, st)
}

func (s *server) inboxDiscard(w http.ResponseWriter, r *http.Request, _ Persona) {
	id, ok := s.inboxRequest(w, r)
	if !ok {
		return
	}
	if _, err := s.cfg.Inbox.Drafts.Take(id); err == nil {
		s.forgetShown(id)
	}
	control.Answer(w, http.StatusOK, s.inboxState())
}

// inboxOutcome is the note on an inbox draft's send: the carrier's answer.
func inboxOutcome(sent *assistant.Sent) string {
	status, ref := "", ""
	if sent.Confirmation != nil {
		status, ref = sent.Confirmation.Status, sent.Confirmation.BookingRef
	}
	return strings.TrimSpace("Sent " + sent.Draft.ID + " to " + sent.Draft.Operator + ", signed. Answer: " + status + " " + ref)
}
