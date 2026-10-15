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

func postStatus(t *testing.T, h http.Handler, body any) (int, OrderStatusResponse) {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/orders/status", strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var resp OrderStatusResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec.Code, resp
}

// TestControlOrderStatus: POST /orders/status asks the forwarder and says
// what it answered; malformed requests are refused before anything is
// sent, and a refused answer is a bad gateway.
func TestControlOrderStatus(t *testing.T) {
	pending := intents.ForwardingConfirmationBody{OrderRef: "MF-0007", Status: intents.StatusPending, SubscriptionID: "SUB-MF-0007"}
	s := shipperWith(t, statusAnswer(found(sentID, pending)))
	s.rememberSent(sentID, meridian, anOrder())
	h := ControlHandler(s, nil)
	code, resp := postStatus(t, h, OrderStatusRequest{Forwarder: meridian, ShipperRef: "CY-PO-55120"})
	if code != http.StatusOK || resp.OrderStatus == nil || !resp.Adopted || resp.Status.OrderRef != "MF-0007" {
		t.Fatalf("status: HTTP %d %+v", code, resp)
	}
	for name, tc := range map[string]struct {
		body any
		want int
	}{
		"unknown field":          {map[string]any{"forwarder": meridian, "shipperRef": "CY-PO-55120", "x": 1}, http.StatusBadRequest},
		"forwarder not a domain": {OrderStatusRequest{Forwarder: "Meridian", ShipperRef: "CY-PO-55120"}, http.StatusBadRequest},
		"asking itself":          {OrderStatusRequest{Forwarder: "cymbal-shoes.example", ShipperRef: "CY-PO-55120"}, http.StatusBadRequest},
		"no reference":           {OrderStatusRequest{Forwarder: meridian}, http.StatusBadRequest},
		"another reference":      {OrderStatusRequest{Forwarder: meridian, ShipperRef: "CY-PO-99999"}, http.StatusBadGateway},
	} {
		if code, resp := postStatus(t, h, tc.body); code != tc.want || resp.Error == "" {
			t.Errorf("%s: HTTP %d %+v, want %d with an error", name, code, resp, tc.want)
		}
	}
}
