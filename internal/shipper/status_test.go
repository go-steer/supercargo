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
	"slices"
	"strings"
	"testing"

	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
)

func statusEvent(id string) intents.TnTEventBody {
	return intents.TnTEventBody{EventID: id, EventType: intents.EventTypeTransport, EventClassifierCode: intents.ClassifierActual,
		EventTypeCode: "DEPA", EventDateTime: "2026-10-07T18:00:00Z", Description: "Departed", UNLocationCode: "NLRTM"}
}

// reporting is a shipper whose operator answers status requests with st
// (its booking reference set to the request's unless ref is given).
func reporting(t *testing.T, ref string, intent string, edit ...func(*intents.TnTStatus)) *Shipper {
	t.Helper()
	s := shipperWith(t, func(h envelope.Header) (string, any) {
		r := *h.BookingRef
		if ref != "" {
			r = ref
		}
		st := intents.TnTStatus{TransportDocumentReference: r,
			DCSAStatus: intents.StatusInTransit, Events: []intents.TnTEventBody{statusEvent("EV-1"), statusEvent("EV-2")}}
		for _, e := range edit {
			e(&st)
		}
		return intent, &intents.TnTStatusResponseBody{TnTResponse: st}
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

func TestStatus(t *testing.T) {
	s := reporting(t, "", intents.OceanTnTResponse)
	st, err := s.Status(context.Background(), "ocean-blue.example", "OB-BKG-1")
	if err != nil {
		t.Fatal(err)
	}
	if st.Status.DCSAStatus != intents.StatusInTransit || !slices.Equal(st.Missing, []string{"EV-2"}) || st.Operator != "ocean-blue.example" {
		t.Errorf("status %+v", st)
	}
	if _, err := s.Status(context.Background(), "ocean-blue.example", "OB-BKG-404"); !errors.Is(err, ErrNotOurBooking) {
		t.Errorf("another booking: %v", err)
	}
	// An answer of another intent is refused by the node; one about
	// another booking by the shipper.
	other := reporting(t, "", intents.TnTInquiryResponse)
	if _, err := other.Status(context.Background(), "ocean-blue.example", "OB-BKG-1"); err == nil {
		t.Error("a status answered by another intent")
	}
	// A report whose body names something other than what was asked.
	for name, s := range map[string]*Shipper{
		"another document": reporting(t, "OB-BKG-2", intents.OceanTnTResponse),
		"another booking":  reporting(t, "", intents.OceanTnTResponse, func(st *intents.TnTStatus) { st.CarrierBookingReference = "OB-BKG-2" }),
		"a container":      reporting(t, "", intents.OceanTnTResponse, func(st *intents.TnTStatus) { st.EquipmentReference = "OCBU0000004" }),
	} {
		if _, err := s.Status(context.Background(), "ocean-blue.example", "OB-BKG-1"); err == nil || !strings.Contains(err.Error(), "references") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestControlStatus(t *testing.T) {
	s := reporting(t, "", intents.OceanTnTResponse)
	h := ControlHandler(s, profiles{})
	post := func(body string) (int, StatusResponse) {
		req := httptest.NewRequest(http.MethodPost, "/status", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var resp StatusResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		return rec.Code, resp
	}
	if code, resp := post(`{"bookingRef":"OB-BKG-1"}`); code != http.StatusOK || resp.Status == nil || len(resp.Missing) != 1 {
		t.Errorf("status: HTTP %d %+v", code, resp)
	}
	for body, want := range map[string]int{
		`{"bookingRef":"OB-BKG-404"}`:                          http.StatusNotFound,
		`{"bookingRef":"OB-BKG-1","operator":"other.example"}`: http.StatusNotFound,
		`{"bookingRef":"OB-BKG-1","x":1}`:                      http.StatusBadRequest,
	} {
		if code, resp := post(body); code != want || resp.Error == "" {
			t.Errorf("%s: HTTP %d %+v, want %d", body, code, resp, want)
		}
	}
	failing := reporting(t, "", intents.TnTInquiryResponse)
	code, resp := func() (int, StatusResponse) {
		req := httptest.NewRequest(http.MethodPost, "/status", strings.NewReader(`{"bookingRef":"OB-BKG-1"}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		ControlHandler(failing, profiles{}).ServeHTTP(rec, req)
		var r StatusResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &r)
		return rec.Code, r
	}()
	if code != http.StatusBadGateway || resp.Error == "" {
		t.Errorf("a failing exchange: HTTP %d %+v", code, resp)
	}
}
