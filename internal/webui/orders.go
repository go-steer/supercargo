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
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/go-steer/supercargo/internal/config"
	"github.com/go-steer/supercargo/internal/control"
	"github.com/go-steer/supercargo/internal/forwarder"
	"github.com/go-steer/supercargo/pkg/intents"
)

// The forwarder's desk (docs/proposals/door-to-door-forwarder.md): a
// person confirms an order's plan, rejects an order, or confirms or
// dismisses a proposal. Each is one button behind the operator role; the
// browser names only what to act on, and the node's own routes decide
// what is sent.

// MaxRejectReason bounds a rejection's reason (a message detail is 512).
const MaxRejectReason = 280

// deskPost runs a forwarder's step on the control API's path, its
// {name} filled from this request's path value of that name, with the
// body body makes (nil: none). The request must come from this UI.
func (s *server) deskPost(path, name string, body func(*http.Request, string) ([]byte, error)) func(http.ResponseWriter, *http.Request, Persona) {
	return func(w http.ResponseWriter, r *http.Request, _ Persona) {
		if !s.sameOrigin(r) {
			control.Answer(w, http.StatusForbidden, errorBody("cross-site request refused"))
			return
		}
		v := r.PathValue(name)
		if v == "" || v == "." || v == ".." {
			control.Answer(w, http.StatusBadRequest, errorBody("not a valid "+name))
			return
		}
		b := []byte("{}")
		if body != nil {
			var err error
			if b, err = body(r, v); err != nil {
				control.Answer(w, http.StatusBadRequest, errorBody(err.Error()))
				return
			}
		}
		target := strings.ReplaceAll(path, "{"+name+"}", v)
		escaped := strings.ReplaceAll(path, "{"+name+"}", url.PathEscape(v))
		r2, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, "/", bytes.NewReader(b)) // a constant method and URL: always builds
		r2.URL = &url.URL{Path: target, RawPath: escaped}
		r2.Header.Set("Content-Type", "application/json")
		s.cfg.Control.ServeHTTP(w, r2)
	}
}

// rejection is a pending order's rejection, with the person's reason.
func rejection(r *http.Request, ref string) ([]byte, error) {
	var req struct {
		Reason string `json:"reason"`
	}
	if err := control.Decode(r, &req); err != nil {
		return nil, err
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		reason = "We can't take this order."
	}
	if utf8.RuneCountInString(reason) > MaxRejectReason {
		return nil, errTooLong
	}
	// The control API's POST /orders/answer (forwarder.AnswerRequest).
	return json.Marshal(struct {
		OrderRef string                             `json:"orderRef"`
		Answer   intents.ForwardingConfirmationBody `json:"answer"`
	}{ref, intents.ForwardingConfirmationBody{Status: intents.StatusRejected, Messages: []intents.Message{{Code: "rejected", Detail: reason}}}})
}

var errTooLong = errors.New("the reason is too long")

// rate is a leg's freight rate for an order, as the person set it.
func rate(r *http.Request, _ string) ([]byte, error) {
	var req forwarder.RateRequest
	if err := control.Decode(r, &req); err != nil {
		return nil, err
	}
	if req.Leg < 1 || req.Leg > config.LaneMaxLegs || !(req.Rate > 0 && req.Rate <= config.MaxRate) {
		return nil, errors.New("a leg (1-8) and a rate over 0, in reais")
	}
	return json.Marshal(req)
}
