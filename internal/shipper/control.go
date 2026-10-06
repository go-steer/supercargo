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
	"net/http"

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
	return mux
}
