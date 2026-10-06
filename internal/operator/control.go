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

// ControlHandler is the operator's control API (internal/control), which
// ulpctl uses to drive the simulator.
func ControlHandler(sim *Simulator) http.Handler {
	mux := http.NewServeMux()
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
