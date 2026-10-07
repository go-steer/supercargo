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

package operator

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-steer/supercargo/internal/control"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/intents"
)

// DisruptRequest is the body of the control API's POST /disrupt.
type DisruptRequest struct {
	BookingRef string `json:"bookingRef"`
	Delay      string `json:"delay"` // a Go duration, e.g. "72h"
	Reason     string `json:"reason"`
}

// DisruptResponse is its answer: the alert published, or why not.
type DisruptResponse struct {
	Alert *intents.TnTAlertBody `json:"alert,omitempty"`
	Error string                `json:"error,omitempty"`
}

// ControlHandler is the operator's control API (internal/control): ulpctl
// drives the simulator through it, and the presenter's views read the
// operator's bookings and what it published.
func ControlHandler(sim *Simulator, bookings *store.Bookings, log *store.EventLog) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /bookings", func(w http.ResponseWriter, _ *http.Request) {
		out := []store.Summary{}
		for _, b := range bookings.List() {
			out = append(out, store.Summarize(b, log.List(b.Operator, b.Ref)))
		}
		control.Answer(w, http.StatusOK, BookingsResponse{Bookings: out})
	})
	mux.HandleFunc("GET /bookings/{ref}/events", func(w http.ResponseWriter, r *http.Request) {
		ref := r.PathValue("ref")
		for _, b := range bookings.List() {
			if b.Ref == ref {
				recs := log.List(b.Operator, b.Ref)
				s := store.Summarize(b, recs)
				control.Answer(w, http.StatusOK, BookingRecordsResponse{Summary: &s, Records: store.ViewRecords(recs)})
				return
			}
		}
		control.Answer(w, http.StatusNotFound, BookingRecordsResponse{Error: "no booking " + ref})
	})
	mux.HandleFunc("POST /disrupt", func(w http.ResponseWriter, r *http.Request) {
		var req DisruptRequest
		if err := control.Decode(r, &req); err != nil {
			control.Answer(w, http.StatusBadRequest, DisruptResponse{Error: err.Error()})
			return
		}
		delay, err := time.ParseDuration(req.Delay)
		if err != nil {
			control.Answer(w, http.StatusBadRequest, DisruptResponse{Error: "delay: " + err.Error()})
			return
		}
		alert, err := sim.Disrupt(req.BookingRef, delay, req.Reason)
		switch {
		case errors.Is(err, ErrUnknownBooking):
			control.Answer(w, http.StatusNotFound, DisruptResponse{Error: err.Error()})
		case errors.Is(err, ErrArrived):
			control.Answer(w, http.StatusConflict, DisruptResponse{Error: err.Error()})
		case err != nil:
			control.Answer(w, http.StatusBadRequest, DisruptResponse{Error: err.Error()})
		default:
			control.Answer(w, http.StatusOK, DisruptResponse{Alert: alert})
		}
	})
	return mux
}

// BookingsResponse is GET /bookings: the operator's bookings, newest first.
type BookingsResponse struct {
	Bookings []store.Summary `json:"bookings"`
}

// BookingRecordsResponse is GET /bookings/{ref}/events: the booking at a
// glance and what was published for it, oldest first.
type BookingRecordsResponse struct {
	Summary *store.Summary     `json:"summary,omitempty"`
	Records []store.RecordView `json:"records,omitempty"`
	Error   string             `json:"error,omitempty"`
}
