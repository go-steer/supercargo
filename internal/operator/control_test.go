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
	"strings"
	"testing"
	"time"
)

func post(t *testing.T, h http.Handler, host, contentType, body string) (int, DisruptResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/disrupt", strings.NewReader(body))
	req.Host = host
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var resp DisruptResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec.Code, resp
}

func TestControlDisrupt(t *testing.T) {
	s := newSimulated(t)
	ref := s.book(t, 1)
	s.advance(2 * time.Second)
	h := ControlHandler(s.sim)
	code, resp := post(t, h, "127.0.0.1:9101", "application/json",
		`{"bookingRef":"`+ref+`","delay":"72h","reason":"Engine repair, unscheduled call at Halifax."}`)
	if code != http.StatusOK || resp.Alert == nil || resp.Alert.NewETA != "2026-10-16T08:00:00Z" {
		t.Fatalf("disrupt: %d %+v", code, resp)
	}
	for name, tc := range map[string]struct {
		host, contentType, body string
		want                    int
	}{
		"unknown booking": {"localhost:9101", "application/json", `{"bookingRef":"OB-BKG-1","delay":"1h","reason":"Late."}`, http.StatusNotFound},
		"bad delay":       {"[::1]:9101", "application/json", `{"bookingRef":"` + ref + `","delay":"soon","reason":"Late."}`, http.StatusBadRequest},
		"bad reason":      {"127.0.0.1:9101", "application/json", `{"bookingRef":"` + ref + `","delay":"1h","reason":""}`, http.StatusBadRequest},
		"unknown field":   {"127.0.0.1:9101", "application/json", `{"bookingRef":"` + ref + `","delay":"1h","reason":"Late.","x":1}`, http.StatusBadRequest},
		"form post":       {"127.0.0.1:9101", "application/x-www-form-urlencoded", `{"bookingRef":"` + ref + `","delay":"1h","reason":"Late."}`, http.StatusBadRequest},
		"text/plain":      {"127.0.0.1:9101", "text/plain", `{"bookingRef":"` + ref + `","delay":"1h","reason":"Late."}`, http.StatusBadRequest},
	} {
		if code, _ := post(t, h, tc.host, tc.contentType, tc.body); code != tc.want {
			t.Errorf("%s: HTTP %d, want %d", name, code, tc.want)
		}
	}
	// Only the first disruption happened: one alert in all.
	alerts := 0
	for _, b := range s.pub.list() {
		if summary(b) == "alert delay" {
			alerts++
		}
	}
	if alerts != 1 {
		t.Errorf("%d alerts published, want 1", alerts)
	}
	// After arrival: 409.
	s2 := newSimulated(t)
	ref2 := s2.book(t, 1)
	s2.advance(time.Second + 162*time.Second) // past the arrival, before gate-out
	if code, _ := post(t, ControlHandler(s2.sim), "127.0.0.1:1", "application/json",
		`{"bookingRef":"`+ref2+`","delay":"1h","reason":"Late."}`); code != http.StatusConflict {
		t.Errorf("after arrival: HTTP %d, want 409", code)
	}
	// Other methods and paths aren't served.
	req := httptest.NewRequest(http.MethodGet, "/disrupt", nil)
	req.Host = "127.0.0.1:9101"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /disrupt: HTTP %d", rec.Code)
	}
}
