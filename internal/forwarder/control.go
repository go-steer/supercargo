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

package forwarder

import (
	"errors"
	"net/http"

	"github.com/go-steer/supercargo/internal/control"
	"github.com/go-steer/supercargo/pkg/intents"
)

// AnswerRequest is the body of the control API's POST /orders/answer: a
// pending order's final answer, which the person at the forwarder has
// already confirmed.
type AnswerRequest struct {
	OrderRef string                             `json:"orderRef"`
	Answer   intents.ForwardingConfirmationBody `json:"answer"`
}

// AnswerResponse is its outcome.
type AnswerResponse struct {
	Answer *intents.ForwardingConfirmationBody `json:"answer,omitempty"`
	Error  string                              `json:"error,omitempty"`
}

// RegisterControl adds the forwarder's routes to mux (internal/control).
// Like the shipper's POST /book, it sends what it is given: the human
// confirmation happens before the call, and reaching the socket takes the
// node user's own access. The orders themselves are listed with the
// carrier side's bookings (GET /bookings).
func RegisterControl(mux *http.ServeMux, o *Orders) {
	mux.HandleFunc("POST /orders/answer", func(w http.ResponseWriter, r *http.Request) {
		var req AnswerRequest
		if err := control.Decode(r, &req); err != nil {
			control.Answer(w, http.StatusBadRequest, AnswerResponse{Error: err.Error()})
			return
		}
		a := req.Answer
		err := o.Answer(r.Context(), req.OrderRef, &a)
		switch {
		case errors.Is(err, ErrUnknownOrder):
			control.Answer(w, http.StatusNotFound, AnswerResponse{Error: err.Error()})
		case errors.Is(err, ErrAnswered):
			control.Answer(w, http.StatusConflict, AnswerResponse{Error: err.Error()})
		case errors.Is(err, ErrInvalidAnswer):
			control.Answer(w, http.StatusBadRequest, AnswerResponse{Error: err.Error()})
		case err != nil:
			control.Answer(w, http.StatusBadGateway, AnswerResponse{Error: err.Error()})
		default:
			control.Answer(w, http.StatusOK, AnswerResponse{Answer: &a})
		}
	})
}
