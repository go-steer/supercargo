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

	"github.com/go-steer/supercargo/pkg/intents"
)

func postOrder(t *testing.T, h http.Handler, body any) (int, OrderResponse) {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/orders", strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var resp OrderResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec.Code, resp
}

// TestControlOrder: POST /orders sends a confirmed order through the node,
// which records the forwarder's answer; malformed requests are refused
// before anything is sent.
func TestControlOrder(t *testing.T) {
	s := shipperWith(t, orderAnswer(intents.ForwardingConfirmationBody{OrderRef: "MF-0007", Status: intents.StatusPending, SubscriptionID: "SUB-MF-0007"}))
	h := ControlHandler(s, nil)
	code, resp := postOrder(t, h, OrderRequest{Forwarder: meridian, Order: *anOrder()})
	if code != http.StatusOK || resp.Answer == nil || resp.Answer.OrderRef != "MF-0007" {
		t.Fatalf("order: HTTP %d %+v", code, resp)
	}
	invalid := *anOrder()
	invalid.Equipment = nil
	for name, tc := range map[string]struct {
		body any
		want int
	}{
		"unknown field":          {map[string]any{"forwarder": meridian, "order": anOrder(), "x": 1}, http.StatusBadRequest},
		"forwarder not a domain": {OrderRequest{Forwarder: "Meridian", Order: *anOrder()}, http.StatusBadRequest},
		"ordering from itself":   {OrderRequest{Forwarder: "cymbal-shoes.example", Order: *anOrder()}, http.StatusBadRequest},
		"invalid order":          {OrderRequest{Forwarder: meridian, Order: invalid}, http.StatusBadRequest},
		"reused reference":       {OrderRequest{Forwarder: meridian, Order: *anOrder()}, http.StatusBadGateway},
	} {
		if code, resp := postOrder(t, h, tc.body); code != tc.want || resp.Error == "" {
			t.Errorf("%s: HTTP %d %+v, want %d with an error", name, code, resp, tc.want)
		}
	}
	s.NoBooking = true
	if code, _ := postOrder(t, h, OrderRequest{Forwarder: meridian, Order: *anOrder()}); code != http.StatusForbidden {
		t.Errorf("a receiver ordering: HTTP %d", code)
	}
}
