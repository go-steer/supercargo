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

package webui

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/supercargo/pkg/intents"
)

// deskUI is a forwarder's UI: it takes orders.
func deskUI(t *testing.T) (http.Handler, *echo) {
	t.Helper()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	ctl := &echo{}
	return Handler(Config{Company: "Meridian Forwarding", Domain: "meridian-forwarding.example", Personas: personas, Control: ctl, Orders: true,
		Now: func() time.Time { return now }}), ctl
}

// TestDeskRoutes: each desk route reaches its control route, for the
// operator role only, from this UI only.
func TestDeskRoutes(t *testing.T) {
	h, ctl := deskUI(t)
	sam, alex := login(t, h, "sam"), login(t, h, "alex")
	for _, c := range []struct{ method, path, body, want string }{
		{http.MethodGet, "/ui/api/orders/MF-0007", "", "/orders/MF-0007"},
		{http.MethodPost, "/ui/api/orders/MF-0007/book", "{}", "/orders/MF-0007/book"},
		{http.MethodPost, "/ui/api/orders/MF-0007/reject", `{"reason":"no capacity"}`, "/orders/answer"},
		{http.MethodPost, "/ui/api/orders/MF-0007/rate", `{"leg":3,"rate":1180}`, "/orders/MF-0007/rate"},
		{http.MethodPost, "/ui/api/proposals/MF-PR-000001/confirm", "{}", "/proposals/MF-PR-000001/confirm"},
		{http.MethodPost, "/ui/api/proposals/MF-PR-000001/dismiss", "{}", "/proposals/MF-PR-000001/dismiss"},
	} {
		ctl.got = nil
		if rec := do(h, c.method, c.path, c.body, sam); rec.Code != http.StatusOK || ctl.got == nil || ctl.got.URL.Path != c.want || ctl.got.Method != c.method {
			t.Errorf("%s %s: HTTP %d, reached %v", c.method, c.path, rec.Code, ctl.got)
		}
		// A shipper-only persona: refused, and nothing reaches the node.
		ctl.got = nil
		if rec := do(h, c.method, c.path, c.body, alex); rec.Code != http.StatusForbidden || ctl.got != nil {
			t.Errorf("%s %s as a shipper: HTTP %d, reached %v", c.method, c.path, rec.Code, ctl.got)
		}
		// Signed out: refused.
		if rec := do(h, c.method, c.path, c.body, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s signed out: HTTP %d", c.method, c.path, rec.Code)
		}
		if c.method != http.MethodPost {
			continue
		}
		// Another site's page: refused, and nothing reaches the node.
		ctl.got = nil
		if rec := do(h, c.method, c.path, c.body, sam, "Origin", "https://evil.example"); rec.Code != http.StatusForbidden || ctl.got != nil {
			t.Errorf("%s %s cross-site: HTTP %d, reached %v", c.method, c.path, rec.Code, ctl.got)
		}
		// A form post (no JSON content type): refused too.
		ctl.got = nil
		if rec := do(h, c.method, c.path, "", sam); rec.Code != http.StatusForbidden || ctl.got != nil {
			t.Errorf("%s %s as a form: HTTP %d, reached %v", c.method, c.path, rec.Code, ctl.got)
		}
	}
}

// TestDeskOffWithoutOrders: a node that takes no orders has no desk.
func TestDeskOffWithoutOrders(t *testing.T) {
	h, ctl, _ := newUI(t)
	sam := login(t, h, "sam")
	for _, path := range []string{"/ui/api/orders/MF-0007/book", "/ui/api/orders/MF-0007/reject", "/ui/api/proposals/P-1/confirm", "/ui/api/proposals/P-1/dismiss"} {
		if rec := do(h, http.MethodPost, path, "{}", sam); rec.Code != http.StatusNotFound || ctl.got != nil {
			t.Errorf("%s: HTTP %d, reached %v", path, rec.Code, ctl.got)
		}
	}
	if rec := do(h, http.MethodGet, "/ui/api/orders/MF-0007", "", sam); rec.Code != http.StatusNotFound || ctl.got != nil {
		t.Errorf("GET an order: HTTP %d, reached %v", rec.Code, ctl.got)
	}
	var me MeResponse
	_ = json.Unmarshal(do(h, http.MethodGet, "/ui/api/me", "", sam).Body.Bytes(), &me)
	if me.Orders {
		t.Error("me.orders on a node without orders")
	}
}

// TestDeskMe: the desk is the operator role's, on a forwarder.
func TestDeskMe(t *testing.T) {
	h, _ := deskUI(t)
	for persona, want := range map[string]bool{"sam": true, "jo": true, "alex": false} {
		var me MeResponse
		_ = json.Unmarshal(do(h, http.MethodGet, "/ui/api/me", "", login(t, h, persona)).Body.Bytes(), &me)
		if me.Orders != want {
			t.Errorf("%s: orders %v, want %v", persona, me.Orders, want)
		}
	}
}

// TestRejection: the browser sends a reason; the UI builds the rejection
// itself, for the order in the path, and nothing else.
func TestRejection(t *testing.T) {
	h, ctl := deskUI(t)
	sam := login(t, h, "sam")
	rec := do(h, http.MethodPost, "/ui/api/orders/MF-0007/reject", `{"reason":"  No capacity on that lane.  "}`, sam)
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d %s", rec.Code, rec.Body)
	}
	var got struct {
		OrderRef string                             `json:"orderRef"`
		Answer   intents.ForwardingConfirmationBody `json:"answer"`
	}
	if err := json.Unmarshal(ctl.body, &got); err != nil {
		t.Fatal(err)
	}
	if got.OrderRef != "MF-0007" || got.Answer.Status != intents.StatusRejected || len(got.Answer.Legs) != 0 ||
		len(got.Answer.Messages) != 1 || got.Answer.Messages[0].Detail != "No capacity on that lane." {
		t.Errorf("sent %+v", got)
	}
	// Valid as the forwarder's answer will check it (it fills the reference).
	got.Answer.OrderRef = got.OrderRef
	if err := got.Answer.Validate(); err != nil {
		t.Errorf("the rejection doesn't validate: %v", err)
	}
	// No reason: a default one.
	ctl.body = nil
	_ = do(h, http.MethodPost, "/ui/api/orders/MF-0007/reject", `{}`, sam)
	if !strings.Contains(string(ctl.body), "can't take this order") {
		t.Errorf("no reason: sent %s", ctl.body)
	}
	// A reason over the cap, or fields it doesn't know (a plan, say): refused.
	for _, body := range []string{`{"reason":"` + strings.Repeat("x", MaxRejectReason+1) + `"}`, `{"reason":"x","answer":{"status":"confirmed"}}`} {
		ctl.got = nil
		if rec := do(h, http.MethodPost, "/ui/api/orders/MF-0007/reject", body, sam); rec.Code != http.StatusBadRequest || ctl.got != nil {
			t.Errorf("%.40s: HTTP %d, reached %v", body, rec.Code, ctl.got)
		}
	}
}

// TestRate: the browser names a leg and a rate; anything else, or out of
// range, is refused before the node.
func TestRate(t *testing.T) {
	h, ctl := deskUI(t)
	sam := login(t, h, "sam")
	if rec := do(h, http.MethodPost, "/ui/api/orders/MF-0007/rate", `{"leg":3,"rate":1180.5}`, sam); rec.Code != http.StatusOK || string(ctl.body) != `{"leg":3,"rate":1180.5}` {
		t.Errorf("HTTP %d, sent %s", rec.Code, ctl.body)
	}
	for _, body := range []string{`{"leg":0,"rate":1180}`, `{"leg":9,"rate":1180}`, `{"leg":3,"rate":0}`, `{"leg":3,"rate":-5}`, `{"leg":3,"rate":1e9}`,
		`{"leg":3,"rate":1180,"booked":"x"}`, `{"leg":"3","rate":1180}`} {
		ctl.got = nil
		if rec := do(h, http.MethodPost, "/ui/api/orders/MF-0007/rate", body, sam); rec.Code != http.StatusBadRequest || ctl.got != nil {
			t.Errorf("%s: HTTP %d, reached %v", body, rec.Code, ctl.got)
		}
	}
}

// TestDeskPaths: a reference that would leave its path segment is refused
// or stays one segment.
func TestDeskPaths(t *testing.T) {
	h, ctl := deskUI(t)
	sam := login(t, h, "sam")
	for _, v := range []string{"%2E%2E", "%2E"} {
		ctl.got = nil
		if rec := do(h, http.MethodPost, "/ui/api/proposals/"+v+"/confirm", "{}", sam); rec.Code != http.StatusBadRequest || ctl.got != nil {
			t.Errorf("%s: HTTP %d, reached %v", v, rec.Code, ctl.got)
		}
	}
	ctl.got = nil
	if rec := do(h, http.MethodPost, "/ui/api/orders/MF%2F0007/book", "{}", sam); rec.Code != http.StatusOK || ctl.got == nil ||
		ctl.got.URL.EscapedPath() != "/orders/MF%2F0007/book" {
		t.Errorf("an escaped slash: HTTP %d, reached %v", rec.Code, ctl.got)
	}
}
