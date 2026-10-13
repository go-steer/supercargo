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

// PlanResponse is the answer to GET /orders/{ref}/plan.
type PlanResponse struct {
	Steps []Step `json:"steps,omitempty"`
	Error string `json:"error,omitempty"`
}

// BookResponse is the answer to POST /orders/{ref}/book: the answer sent
// to the order, or why there is none (and the plan as far as it got).
type BookResponse struct {
	Answer *intents.ForwardingConfirmationBody `json:"answer,omitempty"`
	Steps  []Step                              `json:"steps,omitempty"`
	Error  string                              `json:"error,omitempty"`
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
func RegisterControl(mux *http.ServeMux, o *Orders, orch *Orchestrator) {
	if orch != nil {
		mux.HandleFunc("GET /orders/{ref}/plan", func(w http.ResponseWriter, r *http.Request) {
			steps, err := orch.Plan(r.PathValue("ref"))
			switch {
			case errors.Is(err, ErrUnknownOrder):
				control.Answer(w, http.StatusNotFound, PlanResponse{Error: err.Error()})
			case err != nil:
				control.Answer(w, http.StatusUnprocessableEntity, PlanResponse{Error: err.Error()})
			default:
				control.Answer(w, http.StatusOK, PlanResponse{Steps: steps})
			}
		})
		// The person at the forwarder confirmed the plan: book its legs
		// and answer the order.
		mux.HandleFunc("POST /orders/{ref}/book", func(w http.ResponseWriter, r *http.Request) {
			ref := r.PathValue("ref")
			answer, err := orch.Book(r.Context(), ref)
			steps, _ := orch.Plan(ref)
			switch {
			case errors.Is(err, ErrUnknownOrder):
				control.Answer(w, http.StatusNotFound, BookResponse{Error: err.Error()})
			case errors.Is(err, ErrAnswered):
				control.Answer(w, http.StatusConflict, BookResponse{Error: err.Error(), Steps: steps})
			case errors.Is(err, ErrNoLane), errors.Is(err, ErrEDILeg), errors.Is(err, ErrInvalidAnswer):
				control.Answer(w, http.StatusUnprocessableEntity, BookResponse{Error: err.Error(), Steps: steps})
			case err != nil:
				control.Answer(w, http.StatusBadGateway, BookResponse{Error: err.Error(), Steps: steps})
			default:
				control.Answer(w, http.StatusOK, BookResponse{Answer: answer, Steps: steps})
			}
		})
	}
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
