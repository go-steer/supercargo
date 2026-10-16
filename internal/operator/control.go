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
	"strings"
	"time"

	"github.com/go-steer/supercargo/internal/control"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/intents"
)

// DisruptRequest is the body of the control API's POST /disrupt: one
// booking (BookingRef), or every booking under way on a voyage (Voyage,
// and Vessel if voyage numbers repeat across vessels).
type DisruptRequest struct {
	BookingRef string `json:"bookingRef,omitempty"`
	Vessel     string `json:"vessel,omitempty"`
	Voyage     string `json:"voyage,omitempty"`
	Delay      string `json:"delay"` // a Go duration, e.g. "72h"
	Reason     string `json:"reason"`
}

// DisruptResponse is its answer: the alert published (a booking), each
// booking's outcome (a voyage), or why not. Applied (a booking, HTTP
// 200): the delay took effect but its alert wasn't sent, so disrupting
// again would delay it twice.
type DisruptResponse struct {
	Alert   *intents.TnTAlertBody `json:"alert,omitempty"`
	Alerts  []VoyageAlert         `json:"alerts,omitempty"`
	Applied bool                  `json:"applied,omitempty"`
	Error   string                `json:"error,omitempty"`
}

// BackOfficeResponse is the answer to GET /bookings/{ref}/backoffice.
type BackOfficeResponse struct {
	Steps []store.BackOfficeStep `json:"steps"`
	Error string                 `json:"error,omitempty"`
}

// ControlHandler is the operator's control API (internal/control): ulpctl
// drives the simulator through it, and the presenter's views read the
// operator's bookings and what it published.
func ControlHandler(sim *Simulator, bookings *store.Bookings, log *store.EventLog) http.Handler {
	mux := http.NewServeMux()
	RegisterControl(mux, sim, bookings, log)
	return mux
}

// RegisterControl adds the routes to mux, which may hold another role's:
// a node with several roles serves one control API.
func RegisterControl(mux *http.ServeMux, sim *Simulator, bookings *store.Bookings, log *store.EventLog) {
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
	mux.HandleFunc("GET /bookings/{ref}/journey", func(w http.ResponseWriter, r *http.Request) {
		ref := r.PathValue("ref")
		for _, b := range bookings.List() {
			if b.Ref == ref {
				j := store.BuildJourney(b, log.List(b.Operator, b.Ref))
				control.Answer(w, http.StatusOK, JourneyResponse{Journey: &j})
				return
			}
		}
		control.Answer(w, http.StatusNotFound, JourneyResponse{Error: "no booking " + ref})
	})
	// What the carrier's systems behind the node did for a truck leg: its
	// people's view (the shipper never sees it).
	mux.HandleFunc("GET /bookings/{ref}/backoffice", func(w http.ResponseWriter, r *http.Request) {
		ref := r.PathValue("ref")
		for _, b := range bookings.List() {
			if b.Ref == ref {
				out := BackOfficeResponse{Steps: []store.BackOfficeStep{}}
				if b.Road != nil && b.Road.BackOffice != nil {
					out.Steps = b.Road.BackOffice
				}
				control.Answer(w, http.StatusOK, out)
				return
			}
		}
		control.Answer(w, http.StatusNotFound, BackOfficeResponse{Error: "no booking " + ref})
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
		req.Vessel, req.Voyage = strings.TrimSpace(req.Vessel), strings.TrimSpace(req.Voyage)
		if (req.BookingRef == "") == (req.Voyage == "") || (req.Vessel != "" && req.Voyage == "") {
			control.Answer(w, http.StatusBadRequest, DisruptResponse{Error: "a booking (bookingRef) or a voyage (voyage, and vessel if needed), not both"})
			return
		}
		if req.Voyage != "" {
			alerts, err := sim.DisruptVoyage(req.Vessel, req.Voyage, delay, req.Reason)
			switch {
			case errors.Is(err, ErrUnknownVoyage):
				control.Answer(w, http.StatusNotFound, DisruptResponse{Error: err.Error()})
			case err != nil:
				control.Answer(w, http.StatusBadRequest, DisruptResponse{Error: err.Error()})
			default:
				control.Answer(w, http.StatusOK, DisruptResponse{Alerts: alerts})
			}
			return
		}
		alert, err := sim.Disrupt(req.BookingRef, delay, req.Reason)
		switch {
		case errors.Is(err, ErrUnknownBooking):
			control.Answer(w, http.StatusNotFound, DisruptResponse{Error: err.Error()})
		case errors.Is(err, ErrArrived):
			control.Answer(w, http.StatusConflict, DisruptResponse{Error: err.Error()})
		case errors.Is(err, ErrAlertNotSent):
			// It happened: not a refusal, which a client may retry.
			control.Answer(w, http.StatusOK, DisruptResponse{Applied: true, Error: err.Error()})
		case err != nil:
			control.Answer(w, http.StatusBadRequest, DisruptResponse{Error: err.Error()})
		default:
			control.Answer(w, http.StatusOK, DisruptResponse{Alert: alert})
		}
	})
}

// BookingsResponse is GET /bookings: the operator's bookings, newest first.
type BookingsResponse struct {
	Bookings []store.Summary `json:"bookings"`
}

// JourneyResponse is GET /bookings/{ref}/journey: the booking's journey
// as this carrier published it (docs/ui/route-map.md).
type JourneyResponse struct {
	Journey *store.Journey `json:"journey,omitempty"`
	Error   string         `json:"error,omitempty"`
}

// BookingRecordsResponse is GET /bookings/{ref}/events: the booking at a
// glance and what was published for it, oldest first.
type BookingRecordsResponse struct {
	Summary *store.Summary     `json:"summary,omitempty"`
	Records []store.RecordView `json:"records,omitempty"`
	Error   string             `json:"error,omitempty"`
}
