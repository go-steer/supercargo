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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/store"
)

func TestControlReads(t *testing.T) {
	s := newSimulated(t)
	log := store.NewEventLog(0)
	pub := &Publisher{Domain: "ocean-blue.example", Log: log, Pusher: &fakePusher{}}
	defer pub.Close()
	s.sim.Publisher = pub
	ref := s.book(t, 1)
	s.advance(2 * time.Second) // gate-in
	h := ControlHandler(s.sim, s.svc.Bookings, log)
	get := func(path string, v any) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		return rec.Code
	}
	var br BookingsResponse
	if code := get("/bookings", &br); code != http.StatusOK || len(br.Bookings) != 1 || br.Bookings[0].Ref != ref ||
		br.Bookings[0].Shipper != "cymbal-shoes.example" || br.Bookings[0].Status != "at origin terminal" {
		t.Errorf("bookings: HTTP %d %+v", code, br)
	}
	var rr BookingRecordsResponse
	if code := get("/bookings/"+ref+"/events", &rr); code != http.StatusOK || len(rr.Records) != 1 || rr.Summary.Events != 1 {
		t.Errorf("records: HTTP %d %+v", code, rr)
	}
	if code := get("/bookings/OB-BKG-9/events", &rr); code != http.StatusNotFound || rr.Error == "" {
		t.Errorf("unknown booking: HTTP %d %+v", code, rr)
	}
}
