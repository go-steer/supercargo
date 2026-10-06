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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
)

func postBook(t *testing.T, h http.Handler, contentType string, body any) (int, BookResponse) {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/book", strings.NewReader(string(b)))
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var resp BookResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec.Code, resp
}

func TestControlBook(t *testing.T) {
	s := shipperWith(t, func(envelope.Header) (string, any) {
		return intents.BookingConfirmation, &intents.BookingConfirmationBody{
			BookingRef: "OB-BKG-1", Status: intents.StatusConfirmed,
			Vessel:           &intents.Vessel{Name: "Aurora", Voyage: "42W"},
			PlannedDeparture: "2026-10-07T18:00:00Z", PlannedArrival: "2026-10-13T08:00:00Z",
		}
	})
	h := ControlHandler(s)
	code, resp := postBook(t, h, "application/json", BookRequest{Operator: "ocean-blue.example", Request: *request()})
	if code != http.StatusOK || resp.Confirmation == nil || resp.Confirmation.BookingRef != "OB-BKG-1" {
		t.Fatalf("book: HTTP %d %+v", code, resp)
	}
	// The running node recorded it: this is what lets pushes in.
	if _, err := s.Bookings.Get("ocean-blue.example", "OB-BKG-1"); err != nil {
		t.Errorf("the booking wasn't recorded: %v", err)
	}

	invalid := *request()
	invalid.Equipment = nil
	for name, tc := range map[string]struct {
		contentType string
		body        any
		want        int
	}{
		"form post":             {"application/x-www-form-urlencoded", BookRequest{Operator: "ocean-blue.example", Request: *request()}, http.StatusBadRequest},
		"unknown field":         {"application/json", map[string]any{"operator": "ocean-blue.example", "request": request(), "x": 1}, http.StatusBadRequest},
		"operator not a domain": {"application/json", BookRequest{Operator: "Ocean Blue", Request: *request()}, http.StatusBadRequest},
		"booking with itself":   {"application/json", BookRequest{Operator: "cymbal-shoes.example", Request: *request()}, http.StatusBadRequest},
		"invalid request":       {"application/json", BookRequest{Operator: "ocean-blue.example", Request: invalid}, http.StatusBadRequest},
		// The same reference again: the operator "confirmed" a booking the
		// shipper already has, and Book refuses it.
		"reused reference": {"application/json", BookRequest{Operator: "ocean-blue.example", Request: *request()}, http.StatusBadGateway},
	} {
		if code, resp := postBook(t, h, tc.contentType, tc.body); code != tc.want || resp.Error == "" {
			t.Errorf("%s: HTTP %d %+v, want %d with an error", name, code, resp, tc.want)
		}
	}
}
