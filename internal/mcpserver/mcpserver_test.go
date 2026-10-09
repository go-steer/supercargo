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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func setup(t *testing.T) (*httptest.Server, *assistant.Assistant, *node, *[]activity.Tool) {
	t.Helper()
	n := &node{}
	a := &assistant.Assistant{Node: n, Drafts: &assistant.Drafts{Prefix: "MD"}, Self: assistant.Self{Domain: "cymbal-shoes.example", Name: "Cymbal Shoes"},
		Private: []string{"2026-10-20"}}
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
		// Reads run without asking; drafting and asking a carrier don't.
		ro := tl.Annotations != nil && tl.Annotations.ReadOnlyHint
		if want := tl.Name != assistant.ToolDraftBooking && tl.Name != assistant.ToolAsk; ro != want {
			t.Errorf("%s: read-only %v", tl.Name, ro)
		}
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
	// A question that mentions a private date is refused, and the
	// activity says only that.
	r, _ = cs.CallTool(ctx, &mcp.CallToolParams{Name: assistant.ToolAsk, Arguments: map[string]any{"bookingRef": "OB-BKG-1042", "question": "Will it arrive by 2026-10-20?"}})
	if !r.IsError {
		t.Errorf("a private date went out: %q", text(r))
	}
	// Questions to carriers: MaxAsks node-wide, across sessions, counted
	// on every attempt (the refused ones above included).
	cs2, err := connect(t, srv, token)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs2.Close() }()
	sent := 0
	for i := 0; ; i++ {
		c := cs
		if i%2 == 1 {
			c = cs2
		}
		r, err = c.CallTool(ctx, &mcp.CallToolParams{Name: assistant.ToolAsk, Arguments: map[string]any{"bookingRef": "OB-BKG-1042", "question": "Why is it late?"}})
		if err != nil {
			t.Fatal(err)
		}
		if r.IsError {
			break
		}
		sent++
	}
	if !strings.Contains(text(r), "limit") || sent != MaxAsks-2 || n.asks != sent {
		t.Errorf("past the node's limit: %q, %d sent, %d asked", text(r), sent, n.asks)
	}
	// Every call reached the activity, without arguments or results.
	got := *calls
	if len(got) != 4+sent+1 || got[1].Outcome != "drafted" || got[1].Draft != "MD-0001" || got[2].Reason != "bad_input" ||
		got[3].Reason != "refused" || got[len(got)-1].Reason != "rate_limited" {
		t.Errorf("activity: %+v", got)
	}
	for _, c := range got {
		if strings.Contains(c.Reason, "2026") || strings.Contains(c.Reason, "late") || strings.Contains(c.Reason, "OB-BKG") {
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
	for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
		req, _ := http.NewRequest(method, srv.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized || !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Bearer") {
			t.Errorf("%s with no token: HTTP %d", method, resp.StatusCode)
		}
	}
	// The scheme's case doesn't matter; the token's does.
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	lower := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "bearer "+token)
		return http.DefaultTransport.RoundTrip(r)
	})}
	cs, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: srv.URL, HTTPClient: lower, MaxRetries: -1}, nil)
	if err != nil {
		t.Errorf("a lowercase scheme: %v", err)
	} else {
		_ = cs.Close()
	}
	if _, err := connect(t, srv, strings.ToUpper(token)); err == nil {
		t.Error("a token in another case connected")
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

// TestSessions: idle sessions go, and a new one past the cap replaces
// the least recently used instead of being refused.
func TestSessions(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	s := &server{cfg: Config{Now: func() time.Time { return now }}, sessions: map[string]*session{}}
	a := s.external("a")
	if s.external("a") != a {
		t.Error("a session's state changed between calls")
	}
	for i := range MaxSessions - 1 {
		now = now.Add(time.Second)
		s.external(string(rune('b' + i)))
	}
	now = now.Add(time.Second)
	s.external("a") // "a" is used again: "b" is now the oldest
	now = now.Add(time.Second)
	s.external("new")
	if len(s.sessions) != MaxSessions || s.sessions["b"] != nil || s.sessions["a"] == nil || s.sessions["new"] == nil {
		t.Errorf("eviction: %d sessions, b %v, a %v", len(s.sessions), s.sessions["b"] != nil, s.sessions["a"] != nil)
	}
	now = now.Add(SessionIdle + time.Second)
	s.external("later")
	if len(s.sessions) != 1 {
		t.Errorf("idle sessions kept: %d", len(s.sessions))
	}
}

// TestLimits: the node-wide windows on drafts and on calls reaching other
// companies, and their reset.
func TestLimits(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	s := &server{cfg: Config{Now: func() time.Time { return now }}}
	for range MaxDrafts {
		if !s.allow(assistant.ToolDraftBooking) {
			t.Fatal("a draft within the limit refused")
		}
	}
	if s.allow(assistant.ToolDraftBooking) {
		t.Error("a draft past the limit")
	}
	for range MaxOutbound {
		if !s.allow(assistant.ToolStatus) {
			t.Fatal("a status pull within the limit refused")
		}
	}
	if s.allow(assistant.ToolGetProfile) || !s.allow(assistant.ToolList) {
		t.Error("outbound past the limit, or a local read limited")
	}
	now = now.Add(Window)
	if !s.allow(assistant.ToolDraftBooking) || !s.allow(assistant.ToolStatus) {
		t.Error("the windows didn't reset")
	}
}

// TestVerify: with Verify, each caller's own token is checked, every call
// is logged with who made it, and a session stays its opener's.
func TestVerify(t *testing.T) {
	a := &assistant.Assistant{Node: &node{}, Drafts: &assistant.Drafts{Prefix: "MD"}, Self: assistant.Self{Domain: "cymbal-shoes.example", Name: "Cymbal Shoes"}}
	people := map[string]string{"alice-token": "alice@cymbal.example", "bob-token": "bob@cymbal.example"}
	var mu sync.Mutex
	var calls []activity.Tool
	meta := &Metadata{URL: "https://cymbal.example/.well-known/oauth-protected-resource/mcp", Resource: "https://cymbal.example/mcp",
		AuthorizationServers: []string{"https://accounts.google.com"}, Scopes: []string{"openid", "email"}}
	h, err := Handler(Config{Assistant: a, Name: "cymbal-shoes", Version: "test", Metadata: meta,
		Verify: func(_ context.Context, tok string) (string, time.Time, error) {
			if who, ok := people[tok]; ok {
				return who, time.Now().Add(time.Hour), nil
			}
			if tok == "token-user" {
				return sharedToken, time.Now().Add(time.Hour), nil // must never pass as the shared token
			}
			return "", time.Time{}, errors.New("no")
		},
		OnCall: func(c activity.Tool) { mu.Lock(); defer mu.Unlock(); calls = append(calls, c) }})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	for _, tok := range []string{"", "mallory-token", token, "token-user"} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(resp.Header.Get("WWW-Authenticate"), `resource_metadata="`+meta.URL+`"`) {
			t.Errorf("token %q: %d %q, want 401 naming the metadata", tok, resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
		}
	}

	cs, err := connect(t, srv, "alice-token")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: assistant.ToolList, Arguments: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(calls) != 1 || calls[0].Caller != "alice@cymbal.example" {
		t.Errorf("calls = %+v, want one by alice", calls)
	}
	mu.Unlock()

	// Bob, holding Alice's session ID, is refused.
	body := `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"list_shipments","arguments":{}}}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer bob-token")
	req.Header.Set("Mcp-Session-Id", cs.ID())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("another caller in alice's session: %d, want 403", resp.StatusCode)
	}
	mu.Lock()
	if len(calls) != 1 {
		t.Errorf("bob's call ran: %+v", calls)
	}
	mu.Unlock()
}

func TestMetadata(t *testing.T) {
	m := &Metadata{Resource: "https://cymbal.example/mcp", AuthorizationServers: []string{"https://accounts.google.com"}, Scopes: []string{"openid", "email"}}
	rec := httptest.NewRecorder()
	MetadataHandler(m).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, MetadataPath+Path, nil))
	var got struct {
		Resource string   `json:"resource"`
		Servers  []string `json:"authorization_servers"`
		Scopes   []string `json:"scopes_supported"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Resource != m.Resource || len(got.Servers) != 1 || len(got.Scopes) != 2 {
		t.Errorf("metadata = %d %s", rec.Code, rec.Body)
	}
}
