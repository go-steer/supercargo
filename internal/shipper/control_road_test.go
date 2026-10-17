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

	"github.com/go-steer/supercargo/internal/business"
	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
)

// TestControlBookRoad: a truckload through the control API, its answer or
// why not.
func TestControlBookRoad(t *testing.T) {
	s := shipperWith(t, func(h envelope.Header) (string, any) {
		return intents.RoadBookingConfirmation, &intents.RoadBookingConfirmationBody{BookingRef: "SR-BKG-0913", Status: intents.StatusConfirmed,
			PlannedPickup: "2026-10-20T09:00:00Z", PlannedDelivery: "2026-10-21T15:00:00Z"}
	})
	req := intents.RoadBookingRequestBody{ShipperRef: "VV-1", Shipper: intents.Party{Name: "Cymbal Shoes", Domain: "cymbal-shoes.example"},
		Pickup: intents.Stop{Facility: "Fazenda Boa Safra", UNLocationCode: "BRCGB", PlusCode: "58V75885+8J"}, Delivery: intents.Stop{Facility: "DC", UNLocationCode: "BRGRU"},
		Equipment: []intents.Equipment{{ISOType: "45R1", Count: 1, GrossWeightKg: 26000}}, Cargo: intents.Cargo{Description: "Chilled beef", HSCode: "0201"}}
	body := func(carrier string, r intents.RoadBookingRequestBody) string {
		b, _ := json.Marshal(RoadBookRequest{Carrier: carrier, Request: r})
		return string(b)
	}
	post := func(s *Shipper, b string) int {
		r := httptest.NewRequest(http.MethodPost, "/book/road", strings.NewReader(b))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		ControlHandler(s, nil).ServeHTTP(w, r)
		return w.Code
	}
	bad := req
	bad.Pickup.PlusCode = "5885+8J"
	for _, tc := range []struct {
		body string
		want int
	}{
		{body("ocean-blue.example", req), 200},
		{body("Ocean Blue", req), 400},
		{body("cymbal-shoes.example", req), 400},
		{body("ocean-blue.example", bad), 400},
		{`{`, 400},
	} {
		if got := post(s, tc.body); got != tc.want {
			t.Errorf("%.60s: HTTP %d, want %d", tc.body, got, tc.want)
		}
	}
	s.NoBooking = true
	if got := post(s, body("ocean-blue.example", req)); got != http.StatusForbidden {
		t.Errorf("a receiver: HTTP %d", got)
	}
	failing := shipperWith(t, func(envelope.Header) (string, any) {
		return intents.Error, &intents.ErrorBody{Code: intents.CodeForbidden, Detail: "No."}
	})
	if got := post(failing, body("ocean-blue.example", req)); got != http.StatusBadGateway {
		t.Errorf("refused: HTTP %d", got)
	}
}

// TestControlBookRoadAnswers: a plan that doesn't fit what was asked is a
// conflict (the carrier may hold it); a grant carrying private text, the
// guard's refusal.
func TestControlBookRoadAnswers(t *testing.T) {
	late := shipperWith(t, func(envelope.Header) (string, any) {
		return intents.RoadBookingConfirmation, &intents.RoadBookingConfirmationBody{BookingRef: "SR-BKG-0913", Status: intents.StatusConfirmed,
			PlannedPickup: "2026-10-20T09:00:00Z", PlannedDelivery: "2026-10-25T15:00:00Z"}
	})
	req := intents.RoadBookingRequestBody{ShipperRef: "VV-1", Shipper: intents.Party{Name: "Cymbal Shoes", Domain: "cymbal-shoes.example"},
		Pickup: intents.Stop{Facility: "Farm", UNLocationCode: "BRCGB"}, Delivery: intents.Stop{Facility: "DC", UNLocationCode: "BRGRU", Latest: "2026-10-22T18:00:00Z"},
		Equipment: []intents.Equipment{{ISOType: "45R1", Count: 1, GrossWeightKg: 26000}}, Cargo: intents.Cargo{Description: "Chilled beef", HSCode: "0201"}}
	b, _ := json.Marshal(RoadBookRequest{Carrier: "ocean-blue.example", Request: req})
	r := httptest.NewRequest(http.MethodPost, "/book/road", strings.NewReader(string(b)))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	ControlHandler(late, nil).ServeHTTP(w, r)
	if w.Code != http.StatusConflict {
		t.Errorf("a late plan: HTTP %d %s", w.Code, w.Body)
	}
	guarded := shipperWith(t, func(envelope.Header) (string, any) { return "", nil }, func(c *node.Config) {
		c.OutboundGuard = func(string, []byte) error { return business.ErrLeak }
	})
	_ = guarded.Bookings.Add(store.Booking{Ref: "OB-BKG-1", Operator: "ocean-blue.example", Shipper: "cymbal-shoes.example"})
	r = httptest.NewRequest(http.MethodPost, "/shipments/ocean-blue.example/OB-BKG-1/grant", strings.NewReader(`{"grantee":"aurora.example","scope":["tnt:read"],"days":7,"note":"x"}`))
	r.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	ControlHandler(guarded, nil).ServeHTTP(w, r)
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("a leaking grant: HTTP %d %s", w.Code, w.Body)
	}
}
