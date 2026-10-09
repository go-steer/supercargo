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

package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/go-steer/supercargo/internal/activity"
	"github.com/go-steer/supercargo/internal/assistant"
	"github.com/go-steer/supercargo/internal/shipper"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/intents"
)

const token = "0123456789abcdef0123456789abcdef-test"

// node is the shipper's node as its tools see it.
type node struct {
	mu   sync.Mutex
	asks int
}

func (n *node) Profile(_ context.Context, domain string) (*shipper.Profile, error) {
	return &shipper.Profile{Domain: domain, Name: "Ocean Blue", Intents: []string{intents.BookingRequest}}, nil
}
func (n *node) Shipments(context.Context) ([]store.Summary, error) {
	return []store.Summary{{Ref: "OB-BKG-1042", Operator: "ocean-blue.example", Status: "in transit"}}, nil
}
func (n *node) Records(context.Context, string, string) (*shipper.RecordsResponse, error) {
	return nil, errors.New("no")
}
func (n *node) Ask(_ context.Context, operator, ref, _ string) (*shipper.AskResponse, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.asks++
	return &shipper.AskResponse{Operator: operator, Answer: &intents.TnTInquiryResponseBody{Answer: "Engine repair."}}, nil
}
func (n *node) Status(context.Context, string, string) (*shipper.StatusResponse, error) {
	return nil, errors.New("no")
}
func (n *node) Book(context.Context, shipper.BookRequest) (*shipper.BookResponse, error) {
	return nil, errors.New("tools must never book")
}

// bearer adds the token to every request.
type bearer struct {
	token string
	base  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

func setup(t *testing.T) (*httptest.Server, *assistant.Assistant, *node, *[]activity.Tool) {
	t.Helper()
	n := &node{}
	a := &assistant.Assistant{Node: n, Drafts: &assistant.Drafts{Prefix: "MD"}, Self: assistant.Self{Domain: "cymbal-shoes.example", Name: "Cymbal Shoes"}}
	var mu sync.Mutex
	var calls []activity.Tool
	h, err := Handler(Config{Assistant: a, Token: token, Name: "cymbal-shoes", Version: "test", UIURL: "https://cymbal-shoes.example/ui/",
		OnCall: func(c activity.Tool) { mu.Lock(); defer mu.Unlock(); calls = append(calls, c) }})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, a, n, &calls
}

func connect(t *testing.T, srv *httptest.Server, tok string) (*mcp.ClientSession, error) {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	return c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: srv.URL,
		HTTPClient: &http.Client{Transport: bearer{tok, http.DefaultTransport}}, MaxRetries: -1}, nil)
}

func text(r *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func TestTools(t *testing.T) {
	srv, a, n, calls := setup(t)
	ctx := context.Background()
	cs, err := connect(t, srv, token)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tl := range tools.Tools {
		names[tl.Name] = true
	}
	if !names[assistant.ToolList] || !names[assistant.ToolDraftBooking] || !names[assistant.ToolAsk] || len(tools.Tools) != len(a.ToolList()) {
		t.Errorf("tools: %v", names)
	}
	// A read.
	r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: assistant.ToolList, Arguments: map[string]any{}})
	if err != nil || r.IsError || !strings.Contains(text(r), "OB-BKG-1042") {
		t.Fatalf("list_shipments: %v %v %q", err, r, text(r))
	}
	// A draft: nothing sent, the link to confirm it, the draft in the inbox.
	draft := json.RawMessage(`{"operator":"ocean-blue.example","portOfLoading":"NLRTM","portOfDischarge":"USEWR","grossWeightKg":12000,` +
		`"containerType":"40HC","containerCount":1,"cargoDescription":"Shoes","hsCode":"6404","earliestDeparture":"2026-10-06"}`)
	r, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: assistant.ToolDraftBooking, Arguments: draft})
	if err != nil || r.IsError || !strings.Contains(text(r), "https://cymbal-shoes.example/ui/?draft=MD-0001") || !strings.Contains(text(r), "Nothing has been sent") {
		t.Fatalf("draft_booking: %v %q", err, text(r))
	}
	if open := a.Drafts.Open(); len(open) != 1 || open[0].ID != "MD-0001" {
		t.Errorf("inbox: %+v", open)
	}
	// The checks run on the exact bytes: a case-variant duplicate is refused.
	r, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: assistant.ToolAsk, Arguments: json.RawMessage(`{"bookingRef":"OB-BKG-1042","BookingRef":"x","question":"Why?"}`)})
	if err != nil || !r.IsError {
		t.Errorf("a case-variant duplicate: %v %q", err, text(r))
	}
	// A question goes to the booking's carrier; ten per window per session.
	for i := range assistant.MaxExternalAsks {
		r, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: assistant.ToolAsk, Arguments: map[string]any{"bookingRef": "OB-BKG-1042", "question": "Why is it late?"}})
		if err != nil || r.IsError {
			t.Fatalf("ask %d: %v %q", i, err, text(r))
		}
	}
	r, _ = cs.CallTool(ctx, &mcp.CallToolParams{Name: assistant.ToolAsk, Arguments: map[string]any{"bookingRef": "OB-BKG-1042", "question": "Why is it late?"}})
	if !r.IsError || n.asks != assistant.MaxExternalAsks {
		t.Errorf("the eleventh question: %q, %d asked", text(r), n.asks)
	}
	// Every call reached the activity, without arguments or results.
	got := *calls
	if len(got) != 3+assistant.MaxExternalAsks+1 || got[1].Outcome != "drafted" || got[1].Draft != "MD-0001" || got[2].Outcome != "refused" {
		t.Errorf("activity: %+v", got)
	}
	for _, c := range got {
		if strings.Contains(c.Reason, "Why is it late") {
			t.Errorf("an argument reached the activity: %+v", c)
		}
	}
}

// TestAuth: no token, or the wrong one, gets nowhere.
func TestAuth(t *testing.T) {
	srv, _, _, _ := setup(t)
	if _, err := connect(t, srv, "wrong-token-wrong-token-wrong-token"); err == nil {
		t.Error("a wrong token connected")
	}
	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Bearer") {
		t.Errorf("no token: HTTP %d", resp.StatusCode)
	}
	for name, cfg := range map[string]Config{
		"no assistant": {Token: token},
		"short token":  {Assistant: &assistant.Assistant{}, Token: "short"},
	} {
		if _, err := Handler(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestSessions: idle sessions go, and their number is bounded.
func TestSessions(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	s := &server{cfg: Config{Now: func() time.Time { return now }}, sessions: map[string]*session{}}
	a, _ := s.external("a")
	if b, _ := s.external("a"); b != a {
		t.Error("a session's state changed between calls")
	}
	for i := range MaxSessions - 1 {
		if _, err := s.external(string(rune('b' + i))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.external("one too many"); err == nil {
		t.Error("more than MaxSessions sessions")
	}
	now = now.Add(SessionIdle + time.Second)
	if _, err := s.external("later"); err != nil || len(s.sessions) != 1 {
		t.Errorf("idle sessions kept: %d, %v", len(s.sessions), err)
	}
}
