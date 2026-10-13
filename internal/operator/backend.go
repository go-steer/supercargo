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
	"encoding/json"
	"errors"
	"net/http"
	"slices"

	"github.com/go-steer/supercargo/internal/control"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/intents"
)

// EventRequest is POST /backend/events on the backend API: the carrier's own
// systems (dispatch, telematics, the vessel's systems) telling the node
// that something happened on one of its bookings. The node validates the
// body as strictly as a partner's, records it, and pushes it to the
// booking's subscriber, signed: what the shipment simulator does for the
// demo (docs/proposals/backend-integration.md).
type EventRequest struct {
	BookingRef string          `json:"bookingRef"`
	Intent     string          `json:"intent"` // ulp.tnt.event or ulp.tnt.alert
	Body       json.RawMessage `json:"body"`   // the event or alert, as it will be sent
}

// EventResponse answers POST /backend/events.
type EventResponse struct {
	// ID is the event or alert ID recorded.
	ID string `json:"id,omitempty"`
	// Queued is false when the event was recorded but its push couldn't be
	// queued (the subscriber's queue is full, say): the activity stream
	// says so, and the subscriber can catch up with a status request.
	Queued bool   `json:"queued"`
	Error  string `json:"error,omitempty"`
}

// MaxEventBody bounds an event request's body: an envelope's worth.
const MaxEventBody = 64 << 10

// RegisterBackend adds the backend API's routes for a carrier to mux:
// POST /backend/events. pub records and pushes (the node's Publisher); onPublish,
// if set, learns of each event published (the activity stream).
func RegisterBackend(mux *http.ServeMux, pub Publishing, bookings *store.Bookings, log *store.EventLog, onPublish func(ref string, body intents.Body)) {
	mux.HandleFunc("POST /backend/events", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, MaxEventBody)
		var req EventRequest
		if err := control.Decode(r, &req); err != nil {
			control.Answer(w, http.StatusBadRequest, EventResponse{Error: err.Error()})
			return
		}
		if req.Intent != intents.TnTEvent && req.Intent != intents.TnTAlert {
			control.Answer(w, http.StatusBadRequest, EventResponse{Error: "intent must be " + intents.TnTEvent + " or " + intents.TnTAlert})
			return
		}
		i := slices.IndexFunc(bookings.List(), func(b store.Booking) bool { return b.Ref == req.BookingRef })
		if i < 0 {
			control.Answer(w, http.StatusNotFound, EventResponse{Error: "no booking " + req.BookingRef})
			return
		}
		b := bookings.List()[i]
		body, err := intents.DecodeBody(req.Intent, req.Body)
		if err != nil {
			control.Answer(w, http.StatusBadRequest, EventResponse{Error: err.Error()})
			return
		}
		// An alert may only cite events this booking already has: the
		// shipper checks the same before believing it.
		if a, ok := body.(*intents.TnTAlertBody); ok {
			held := map[string]bool{}
			for _, rec := range log.List(b.Operator, b.Ref) {
				held[rec.ID] = true
			}
			for _, c := range a.CauseEventIDs {
				if !held[c] {
					control.Answer(w, http.StatusBadRequest, EventResponse{Error: "the alert cites " + c + ", which isn't an event of " + b.Ref})
					return
				}
			}
		}
		id := pushID(body)
		err = pub.Publish(b, body)
		switch {
		case errors.Is(err, ErrDuplicate):
			control.Answer(w, http.StatusConflict, EventResponse{ID: id, Error: err.Error()})
			return
		case errors.Is(err, ErrClosed):
			control.Answer(w, http.StatusServiceUnavailable, EventResponse{Error: err.Error()})
			return
		case errors.Is(err, ErrNotQueued):
			// Recorded: not a refusal, which a client may retry.
		case err != nil:
			control.Answer(w, http.StatusBadRequest, EventResponse{Error: err.Error()})
			return
		}
		if onPublish != nil {
			onPublish(b.Ref, body)
		}
		control.Answer(w, http.StatusOK, EventResponse{ID: id, Queued: err == nil})
	})
}
