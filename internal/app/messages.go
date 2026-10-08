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

package app

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/go-steer/supercargo/internal/activity"
	"github.com/go-steer/supercargo/internal/control"
	"github.com/go-steer/supercargo/internal/store"
)

// MessagesResponse answers GET /messages; MessageResponse GET /messages/{seq}.
type MessagesResponse struct {
	Messages []store.Message `json:"messages"`
	Error    string          `json:"error,omitempty"`
}

// MessageResponse is one message, with every byte as it was on the wire.
type MessageResponse struct {
	Message *store.Message `json:"message,omitempty"`
	Error   string         `json:"error,omitempty"`
}

// MaxMessages is the most messages one GET /messages returns.
const MaxMessages = 500

// sidesOf reads ?side= (repeatable): operator or customer.
func sidesOf(r *http.Request) ([]string, error) {
	sides := r.URL.Query()["side"]
	for _, s := range sides {
		if s != activity.SideOperator && s != activity.SideCustomer {
			return nil, errors.New("side must be operator or customer")
		}
	}
	return sides, nil
}

// messagesHandler serves the message log: ?side= keeps those sides'
// messages and the whole node's, ?since=N those after N, ?conversation=
// one correlationId's, ?limit= the newest that many (at most
// MaxMessages, the default).
func (a *App) messagesHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fail := func(msg string) { control.Answer(w, http.StatusBadRequest, MessagesResponse{Error: msg}) }
		sides, err := sidesOf(r)
		if err != nil {
			fail(err.Error())
			return
		}
		q := r.URL.Query()
		f := store.MessageFilter{Sides: sides, Conversation: q.Get("conversation"), Limit: MaxMessages}
		if s := q.Get("since"); s != "" {
			if f.Since, err = strconv.ParseUint(s, 10, 64); err != nil {
				fail("since must be a sequence number")
				return
			}
		}
		if s := q.Get("limit"); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil || n < 1 || n > MaxMessages {
				fail("limit must be 1-" + strconv.Itoa(MaxMessages))
				return
			}
			f.Limit = n
		}
		control.Answer(w, http.StatusOK, MessagesResponse{Messages: a.Messages.List(f)})
	})
}

// messageHandler serves one message by sequence number; ?side= as for
// the list. A message of another side is not found, like one never kept.
func (a *App) messageHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sides, err := sidesOf(r)
		if err != nil {
			control.Answer(w, http.StatusBadRequest, MessageResponse{Error: err.Error()})
			return
		}
		seq, err := strconv.ParseUint(r.PathValue("seq"), 10, 64)
		if err != nil {
			control.Answer(w, http.StatusBadRequest, MessageResponse{Error: "not a sequence number"})
			return
		}
		m, ok := a.Messages.Get(seq, sides)
		if !ok {
			control.Answer(w, http.StatusNotFound, MessageResponse{Error: "no message " + strconv.FormatUint(seq, 10)})
			return
		}
		control.Answer(w, http.StatusOK, MessageResponse{Message: &m})
	})
}
