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

package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-steer/supercargo/internal/config"
	"github.com/go-steer/supercargo/internal/geo"
	"github.com/go-steer/supercargo/internal/operator"
	"github.com/go-steer/supercargo/internal/staticmap"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/intents"
)

// TestLegMap: a truck leg's map is drawn once from its ends' Plus Codes
// and the corridor, and served as a PNG; an unknown leg, an ocean booking
// and a failed drawing aren't.
func TestLegMap(t *testing.T) {
	t.Setenv("SC_TEST_MAPS", "")
	if _, err := newLegMap(&config.GoogleWeather{APIKeyEnv: "SC_TEST_MAPS"}, nil, nil); err == nil || !strings.Contains(err.Error(), "$SC_TEST_MAPS") {
		t.Errorf("no key: %v", err)
	}
	if m, err := newLegMap(nil, nil, nil); m != nil || err != nil {
		t.Errorf("no map: %v %v", m, err)
	}
	t.Setenv("SC_TEST_MAPS", "k")
	bookings := store.NewBookings()
	_ = bookings.Add(store.Booking{Ref: "SR-1", Operator: "sr.example", Shipper: "vv.example", Road: &store.RoadBooking{Request: intents.RoadBookingRequestBody{
		Pickup: intents.Stop{PlusCode: "58V75885+8J"}, Delivery: intents.Stop{PlusCode: "588MG3PP+QM"}}}})
	_ = bookings.Add(store.Booking{Ref: "SR-2", Operator: "sr.example", Shipper: "vv.example", Road: &store.RoadBooking{Request: intents.RoadBookingRequestBody{
		Pickup: intents.Stop{PlusCode: "58V75885+8J"}}}})
	_ = bookings.Add(store.Booking{Ref: "OB-1", Operator: "sr.example", Shipper: "vv.example"})
	m, err := newLegMap(&config.GoogleWeather{APIKeyEnv: "SC_TEST_MAPS"}, []operator.Waypoint{{Name: "Sinop", Point: geo.Point{Lat: -11.86, Lng: -55.5}}}, bookings)
	if err != nil {
		t.Fatal(err)
	}
	var drawn []staticmap.Map
	m.render = func(_ context.Context, d staticmap.Map) ([]byte, error) {
		drawn = append(drawn, d)
		if d.Delivery == nil {
			return nil, staticmap.ErrNothing
		}
		return []byte("\x89PNG..."), nil
	}
	get := func(ref string) *httptest.ResponseRecorder {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /bookings/{ref}/map", m.serve)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/bookings/"+ref+"/map", nil))
		return w
	}
	for range 2 {
		if w := get("SR-1"); w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || w.Body.String() != "\x89PNG..." {
			t.Fatalf("SR-1: %d %q", w.Code, w.Body)
		}
	}
	if len(drawn) != 1 || drawn[0].Pickup == nil || len(drawn[0].Via) != 1 {
		t.Errorf("drawn %+v", drawn)
	}
	if w := get("SR-2"); w.Code != http.StatusNotFound || len(drawn) != 2 || drawn[1].Via != nil {
		t.Errorf("one end: %d, drawn %+v", w.Code, drawn)
	}
	// A failure stands a minute: the API isn't asked again on every refresh.
	m.render = func(_ context.Context, d staticmap.Map) ([]byte, error) {
		drawn = append(drawn, d)
		return nil, staticmap.ErrFailed
	}
	m.cache = map[string][]byte{}
	for range 3 {
		if w := get("SR-1"); w.Code != http.StatusBadGateway {
			t.Errorf("failing: %d", w.Code)
		}
	}
	if len(drawn) != 3 {
		t.Errorf("asked %d times while failing", len(drawn)-2)
	}
	full := map[string]int{}
	for i := range maxMaps {
		full[string(rune(i))] = i
	}
	evictOne(full)
	if len(full) != maxMaps-1 {
		t.Errorf("evicted to %d", len(full))
	}
	for _, ref := range []string{"OB-1", "SR-404"} {
		if w := get(ref); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d", ref, w.Code)
		}
	}
}
