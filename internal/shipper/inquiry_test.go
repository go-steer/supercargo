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
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
)

// answering is a shipper whose operator answers every inquiry with resp
// (its dataScope set to the asked booking unless scope is given), holding
// OB-BKG-1 with ocean-blue.example and a recorded event EV-1.
func answering(t *testing.T, answer string, cited []string, scope string) *Shipper {
	t.Helper()
	s := shipperWith(t, func(h envelope.Header) (string, any) {
		ref := scope
		if ref == "" {
			ref = *h.BookingRef
		}
		return intents.TnTInquiryResponse, &intents.TnTInquiryResponseBody{Answer: answer, CitedEventIDs: cited,
			GeneratedBy: intents.GeneratedByLLM, DataScope: intents.DataScope{BookingRef: ref, EventsConsidered: 2}}
	})
	if err := s.Bookings.Add(store.Booking{Ref: "OB-BKG-1", Operator: "ocean-blue.example", Shipper: "cymbal-shoes.example"}); err != nil {
		t.Fatal(err)
	}
	s.Events = store.NewEventLog(0)
	if _, err := s.Events.Add("ocean-blue.example", "OB-BKG-1", store.Record{ID: "EV-1", Intent: intents.TnTEvent}); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAsk(t *testing.T) {
	s := answering(t, "Departed on the 7th.", []string{"EV-1", "EV-2"}, "")
	a, err := s.Ask(context.Background(), "ocean-blue.example", "OB-BKG-1", "Did it leave?")
	if err != nil {
		t.Fatal(err)
	}
	if a.Response.Answer != "Departed on the 7th." || strings.Join(a.Held, ",") != "EV-1" || a.Operator != "ocean-blue.example" {
		t.Errorf("answer %+v", a)
	}
	// Without an event log, nothing is marked held.
	s.Events = nil
	if a, _ := s.Ask(context.Background(), "ocean-blue.example", "OB-BKG-1", "Did it leave?"); a == nil || a.Held != nil {
		t.Errorf("no event log: %+v", a)
	}
}

func TestAskRejects(t *testing.T) {
	s := answering(t, "Departed.", nil, "")
	if _, err := s.Ask(context.Background(), "ocean-blue.example", "OB-BKG-9", "Where?"); !errors.Is(err, ErrNotOurBooking) {
		t.Errorf("a booking not held: %v", err)
	}
	if _, err := s.Ask(context.Background(), "other.example", "OB-BKG-1", "Where?"); !errors.Is(err, ErrNotOurBooking) {
		t.Errorf("the booking under another operator: %v", err)
	}
	if _, err := s.Ask(context.Background(), "ocean-blue.example", "OB-BKG-1", ""); err == nil {
		t.Error("an empty question was sent")
	}
	// An answer about another booking is refused, however well signed.
	wrong := answering(t, "Elsewhere.", nil, "OB-BKG-2")
	if _, err := wrong.Ask(context.Background(), "ocean-blue.example", "OB-BKG-1", "Where?"); err == nil {
		t.Error("an answer whose dataScope is another booking was accepted")
	}
}

func TestControlAsk(t *testing.T) {
	s := answering(t, "Departed.", []string{"EV-1"}, "")
	h := ControlHandler(s, nil)
	post := func(body any) (int, AskResponse) {
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/ask", strings.NewReader(string(b)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var resp AskResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		return rec.Code, resp
	}
	code, resp := post(AskRequest{BookingRef: "OB-BKG-1", Question: "Did it leave?"})
	if code != http.StatusOK || resp.Operator != "ocean-blue.example" || resp.Answer == nil || strings.Join(resp.Held, ",") != "EV-1" {
		t.Fatalf("ask: HTTP %d %+v", code, resp)
	}
	if code, _ := post(AskRequest{Operator: "ocean-blue.example", BookingRef: "OB-BKG-1", Question: "Did it leave?"}); code != http.StatusOK {
		t.Errorf("with the operator named: HTTP %d", code)
	}
	if code, resp := post(AskRequest{BookingRef: "OB-BKG-9", Question: "Where?"}); code != http.StatusNotFound || resp.Error == "" {
		t.Errorf("unknown booking: HTTP %d %+v", code, resp)
	}
	if code, _ := post(AskRequest{Operator: "other.example", BookingRef: "OB-BKG-1", Question: "Where?"}); code != http.StatusNotFound {
		t.Errorf("booking under another operator: HTTP %d", code)
	}
	if code, _ := post(map[string]any{"bookingRef": "OB-BKG-1", "question": "Where?", "x": 1}); code != http.StatusBadRequest {
		t.Errorf("unknown field: HTTP %d", code)
	}
	if code, _ := post(AskRequest{BookingRef: "OB-BKG-1", Question: ""}); code != http.StatusBadGateway {
		t.Errorf("empty question: HTTP %d", code)
	}
	// Two operators share the reference: the operator must be named.
	_ = s.Bookings.Add(store.Booking{Ref: "OB-BKG-1", Operator: "another.example", Shipper: "cymbal-shoes.example"})
	if code, resp := post(AskRequest{BookingRef: "OB-BKG-1", Question: "Where?"}); code != http.StatusConflict || !strings.Contains(resp.Error, "another.example") {
		t.Errorf("ambiguous reference: HTTP %d %+v", code, resp)
	}
}
