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

package shipper

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-steer/supercargo/internal/control"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
)

// BookRequest is the body of the control API's POST /book: a booking the
// person at the CLI has already confirmed.
type BookRequest struct {
	Operator string                     `json:"operator"`
	Request  intents.BookingRequestBody `json:"request"`
}

// BookResponse is its answer: the operator's confirmation, or why there is
// none.
type BookResponse struct {
	Confirmation *intents.BookingConfirmationBody `json:"confirmation,omitempty"`
	Error        string                           `json:"error,omitempty"`
}

// ControlHandler is the shipper's control API (internal/control). Booking
// goes through the running node so the node records the booking, and so
// accepts the operator's pushes for it.
//
// It sends whatever it is given: the human confirmation the hard rules
// require happens in the CLI before the call, and reaching the socket at
// all takes the node user's own access.
func ControlHandler(s *Shipper) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /book", func(w http.ResponseWriter, r *http.Request) {
		var req BookRequest
		if err := control.Decode(r, &req); err != nil {
			control.Answer(w, http.StatusBadRequest, BookResponse{Error: err.Error()})
			return
		}
		if !envelope.ValidDomain(req.Operator) || req.Operator == s.Node.Domain() {
			control.Answer(w, http.StatusBadRequest, BookResponse{Error: "operator must be another party's lowercase domain"})
			return
		}
		if err := req.Request.Validate(); err != nil {
			control.Answer(w, http.StatusBadRequest, BookResponse{Error: "request: " + err.Error()})
			return
		}
		conf, err := s.Book(r.Context(), req.Operator, &req.Request)
		if err != nil {
			control.Answer(w, http.StatusBadGateway, BookResponse{Error: err.Error()})
			return
		}
		control.Answer(w, http.StatusOK, BookResponse{Confirmation: conf})
	})
	mux.HandleFunc("POST /ask", func(w http.ResponseWriter, r *http.Request) {
		var req AskRequest
		if err := control.Decode(r, &req); err != nil {
			control.Answer(w, http.StatusBadRequest, AskResponse{Error: err.Error()})
			return
		}
		op := req.Operator
		if op == "" {
			// The reference alone names it if exactly one operator has it.
			switch ops := s.Bookings.Operators(req.BookingRef); len(ops) {
			case 0:
				control.Answer(w, http.StatusNotFound, AskResponse{Error: "no booking " + req.BookingRef})
				return
			case 1:
				op = ops[0]
			default:
				control.Answer(w, http.StatusConflict, AskResponse{Error: "several operators have a booking " + req.BookingRef + "; name one: " + strings.Join(ops, ", ")})
				return
			}
		}
		a, err := s.Ask(r.Context(), op, req.BookingRef, req.Question)
		switch {
		case errors.Is(err, ErrNotOurBooking):
			control.Answer(w, http.StatusNotFound, AskResponse{Error: err.Error()})
		case err != nil:
			control.Answer(w, http.StatusBadGateway, AskResponse{Error: err.Error()})
		default:
			control.Answer(w, http.StatusOK, AskResponse{Operator: a.Operator, Answer: a.Response, Held: a.Held})
		}
	})
	return mux
}

// AskRequest is the body of the control API's POST /ask. Operator may be
// empty when only one operator has a booking under BookingRef.
type AskRequest struct {
	Operator   string `json:"operator,omitempty"`
	BookingRef string `json:"bookingRef"`
	Question   string `json:"question"`
}

// AskResponse is its answer.
type AskResponse struct {
	Operator string                          `json:"operator,omitempty"`
	Answer   *intents.TnTInquiryResponseBody `json:"answer,omitempty"`
	Held     []string                        `json:"held,omitempty"`
	Error    string                          `json:"error,omitempty"`
}
