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
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/assistant"
)

func inboxUI(t *testing.T) (http.Handler, *assistant.Assistant, *node, *time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	n := &node{}
	inbox := &assistant.Assistant{Node: n, Drafts: &assistant.Drafts{Prefix: "MD"}, Now: clock,
		Self: assistant.Self{Domain: "cymbal-shoes.example", Name: "Cymbal Shoes"}}
	h := Handler(Config{Company: "Cymbal Shoes", Domain: "cymbal-shoes.example", Personas: personas, Control: &echo{}, Now: clock, Inbox: inbox})
	return h, inbox, n, &now
}

func inboxPost(h http.Handler, path, id string, c *http.Cookie, hdr ...string) (int, InboxResponse) {
	b, _ := json.Marshal(SendRequest{DraftID: id})
	rec := do(h, http.MethodPost, path, string(b), c, hdr...)
	var r InboxResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &r)
	return rec.Code, r
}

// TestInbox: an outside agent's draft waits for a shipper's persona, who
// sees it, then confirms it after the arming delay; every other way in is
// refused.
func TestInbox(t *testing.T) {
	h, inbox, n, now := inboxUI(t)
	draft := json.RawMessage(`{"operator":"ocean-blue.example","shipperRef":"CY-PO-7","portOfLoading":"NLRTM","portOfDischarge":"USEWR","grossWeightKg":12000,` +
		`"containerType":"40HC","containerCount":1,"cargoDescription":"Shoes","hsCode":"6404","earliestDeparture":"2026-10-06"}`)
	if out := inbox.RunTool(context.Background(), assistant.NewExternal(), assistant.ToolDraftBooking, draft); out.Draft == nil || out.Draft.DraftID != "MD-0001" {
		t.Fatalf("an outside agent's draft: %+v", out)
	}
	alex, sam := login(t, h, "alex"), login(t, h, "sam")
	// Not shown yet: too early to send.
	if code, _ := inboxPost(h, "/ui/api/inbox/send", "MD-0001", alex); code != http.StatusTooEarly {
		t.Errorf("a send before it was shown: HTTP %d", code)
	}
	var r InboxResponse
	_ = json.Unmarshal(do(h, http.MethodGet, "/ui/api/inbox", "", alex).Body.Bytes(), &r)
	if len(r.Drafts) != 1 || r.Drafts[0].DraftID != "MD-0001" {
		t.Fatalf("inbox: %+v", r)
	}
	// Listing the inbox, or another session's dialog, starts nothing for
	// this session; only its own dialog's fetch does.
	jo := login(t, h, "jo")
	_ = do(h, http.MethodGet, "/ui/api/inbox/draft?id=MD-0001", "", jo)
	*now = now.Add(ArmDelay)
	if code, _ := inboxPost(h, "/ui/api/inbox/send", "MD-0001", alex); code != http.StatusTooEarly {
		t.Errorf("a send armed by another page: HTTP %d", code)
	}
	if rec := do(h, http.MethodGet, "/ui/api/inbox/draft?id=MD-0001", "", alex); rec.Code != http.StatusOK {
		t.Fatalf("the dialog's fetch: HTTP %d", rec.Code)
	}
	if code, _ := inboxPost(h, "/ui/api/inbox/send", "MD-0001", alex); code != http.StatusTooEarly {
		t.Errorf("a send at once: HTTP %d", code)
	}
	if rec := do(h, http.MethodGet, "/ui/api/inbox/draft?id=MD-0099", "", alex); rec.Code != http.StatusNotFound {
		t.Errorf("an unknown draft's fetch: HTTP %d", rec.Code)
	}
	*now = now.Add(ArmDelay)
	for name, c := range map[string]struct {
		cookie *http.Cookie
		id     string
		hdr    []string
		want   int
	}{
		"the carrier persona": {sam, "MD-0001", nil, http.StatusForbidden},
		"no session":          {nil, "MD-0001", nil, http.StatusUnauthorized},
		"cross-site":          {alex, "MD-0001", []string{"Origin", "https://evil.example"}, http.StatusForbidden},
		"unknown draft":       {alex, "MD-0099", nil, http.StatusNotFound},
	} {
		if code, _ := inboxPost(h, "/ui/api/inbox/send", c.id, c.cookie, c.hdr...); code != c.want {
			t.Errorf("%s: HTTP %d, want %d", name, code, c.want)
		}
	}
	if rec := do(h, http.MethodGet, "/ui/api/inbox", "", sam); rec.Code != http.StatusForbidden {
		t.Errorf("the carrier reading the inbox: HTTP %d", rec.Code)
	}
	if len(n.booked) != 0 {
		t.Fatal("a refused send booked")
	}
	code, r := inboxPost(h, "/ui/api/inbox/send", "MD-0001", alex)
	if code != http.StatusOK || len(n.booked) != 1 || r.Note != "Sent MD-0001 to ocean-blue.example, signed. Answer: confirmed OB-BKG-1" || len(r.Drafts) != 0 {
		t.Fatalf("send: HTTP %d %+v, %d booked", code, r, len(n.booked))
	}
	if code, _ := inboxPost(h, "/ui/api/inbox/send", "MD-0001", alex); code != http.StatusNotFound || len(n.booked) != 1 {
		t.Errorf("a second send: HTTP %d", code)
	}
	// Discarded: gone, never sent.
	inbox.RunTool(context.Background(), assistant.NewExternal(), assistant.ToolDraftBooking, draft)
	if code, r := inboxPost(h, "/ui/api/inbox/discard", "MD-0002", alex); code != http.StatusOK || len(r.Drafts) != 0 {
		t.Errorf("discard: HTTP %d %+v", code, r)
	}
	if code, _ := inboxPost(h, "/ui/api/inbox/send", "MD-0002", alex); code != http.StatusNotFound || len(n.booked) != 1 {
		t.Errorf("a discarded draft was sent: HTTP %d", code)
	}
	// A failed send says so.
	n.fail = &assistant.NodeError{Status: http.StatusBadRequest, Message: "refused"}
	inbox.RunTool(context.Background(), assistant.NewExternal(), assistant.ToolDraftBooking, draft)
	_ = do(h, http.MethodGet, "/ui/api/inbox/draft?id=MD-0003", "", alex)
	*now = now.Add(ArmDelay)
	if code, r := inboxPost(h, "/ui/api/inbox/send", "MD-0003", alex); code != http.StatusBadGateway || r.Error == "" {
		t.Errorf("a refused send: HTTP %d %+v", code, r)
	}
	// Malformed requests.
	if rec := do(h, http.MethodPost, "/ui/api/inbox/send", `{"draftId":1}`, alex); rec.Code != http.StatusBadRequest {
		t.Errorf("a malformed send: HTTP %d", rec.Code)
	}
	if code, _ := inboxPost(h, "/ui/api/inbox/discard", "MD-0099", alex); code != http.StatusNotFound {
		t.Errorf("discarding an unknown draft: HTTP %d", code)
	}
	// A node without an inbox.
	plain, _, _ := newUI(t)
	c := login(t, plain, "alex")
	var empty InboxResponse
	_ = json.Unmarshal(do(plain, http.MethodGet, "/ui/api/inbox", "", c).Body.Bytes(), &empty)
	if len(empty.Drafts) != 0 {
		t.Errorf("no inbox: %+v", empty)
	}
	if code, _ := inboxPost(plain, "/ui/api/inbox/send", "MD-0001", c); code != http.StatusConflict {
		t.Errorf("a send with no inbox: HTTP %d", code)
	}
	if rec := do(plain, http.MethodGet, "/ui/api/inbox/draft?id=MD-0001", "", c); rec.Code != http.StatusNotFound {
		t.Errorf("a draft with no inbox: HTTP %d", rec.Code)
	}
}
