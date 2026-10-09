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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/go-steer/supercargo/internal/activity"
	"github.com/go-steer/supercargo/internal/assistant"
	"github.com/go-steer/supercargo/internal/config"
	"github.com/go-steer/supercargo/internal/webui"
)

const testMCPToken = "an-mcp-token-for-the-test-of-32-bytes"

type withToken struct{ base http.RoundTripper }

func (b withToken) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+testMCPToken)
	return b.base.RoundTrip(r)
}

// TestMCPDraftConfirmedInUI: an outside agent drafts a booking over MCP on
// the shipper's node; nothing is sent until a person confirms the draft in
// the browser UI's inbox, and then the booking goes to the operator over
// the protocol.
func TestMCPDraftConfirmedInUI(t *testing.T) {
	webui.ArmDelay = 0
	defer func() { webui.ArmDelay = 1500 * time.Millisecond }()
	t.Setenv("SUPERCARGO_TEST_MCP_TOKEN", testMCPToken)
	p := startPairWith(t, "", `role: shipper
mcp: { server: { tokenEnv: SUPERCARGO_TEST_MCP_TOKEN, uiURL: "https://cymbal-shoes.example/ui/" } }
web:
  auth: demo
  personas:
    - { id: alex, name: Alex, roles: [shipper] }`)
	srv := httptest.NewServer(p.shipper.Server.Handler)
	defer srv.Close()

	c := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "1"}, nil)
	cs, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp",
		HTTPClient: &http.Client{Transport: withToken{http.DefaultTransport}}, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	draft := json.RawMessage(`{"operator":"ocean-blue.example","shipperRef":"CY-PO-7","portOfLoading":"NLRTM","portOfDischarge":"USEWR","grossWeightKg":12000,` +
		`"containerType":"40HC","containerCount":1,"cargoDescription":"Shoes","hsCode":"6404","earliestDeparture":"2026-10-06"}`)
	r, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: assistant.ToolDraftBooking, Arguments: draft})
	if err != nil || r.IsError {
		t.Fatalf("draft over MCP: %v %+v", err, r)
	}
	if p.operator.Carrier.Bookings.Len() != 0 {
		t.Fatal("an MCP draft reached the operator")
	}
	// The tool call is in the shipper's activity, as a draft.
	past, _, _, cancel := p.shipper.Activity.Subscribe(0, 0)
	cancel()
	found := false
	for _, e := range past {
		if e.Kind == activity.KindTool && e.Tool.Outcome == "drafted" && e.Tool.Draft == "MD-0001" && e.Side == activity.SideCustomer {
			found = true
		}
	}
	if !found {
		t.Error("the MCP tool call isn't in the activity")
	}

	// A person confirms it in the browser UI.
	ui := func(method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Host = "cymbal-shoes.localhost"
		req.Header.Set("Content-Type", "application/json")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		p.shipper.Server.Handler.ServeHTTP(rec, req)
		return rec
	}
	cookie := ui(http.MethodPost, "/ui/api/login", `{"persona":"alex"}`, nil).Result().Cookies()[0]
	var inbox webui.InboxResponse
	_ = json.Unmarshal(ui(http.MethodGet, "/ui/api/inbox", "", cookie).Body.Bytes(), &inbox)
	if len(inbox.Drafts) != 1 || inbox.Drafts[0].DraftID != "MD-0001" {
		t.Fatalf("inbox: %+v", inbox)
	}
	rec := ui(http.MethodPost, "/ui/api/inbox/send", `{"draftId":"MD-0001"}`, cookie)
	_ = json.Unmarshal(rec.Body.Bytes(), &inbox)
	if rec.Code != http.StatusOK || !strings.Contains(inbox.Note, "Sent MD-0001 to ocean-blue.example") || len(inbox.Drafts) != 0 {
		t.Fatalf("send: HTTP %d %+v", rec.Code, inbox)
	}
	if p.operator.Carrier.Bookings.Len() != 1 || p.shipper.Customer.Bookings.Len() != 1 {
		t.Errorf("bookings: operator %d, shipper %d", p.operator.Carrier.Bookings.Len(), p.shipper.Customer.Bookings.Len())
	}
}

// TestMCPConfig: the endpoint is served only when configured, and a node
// with too short a token doesn't start.
func TestMCPConfig(t *testing.T) {
	c := cfg(t, config.RoleShipper)
	a, err := New(c, quiet())
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	a.Server.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}")))
	if rec.Code == http.StatusUnauthorized {
		t.Error("an unconfigured node serves /mcp")
	}
	t.Setenv("SUPERCARGO_TEST_MCP_TOKEN", "short")
	c.MCP = &config.MCP{Server: &config.MCPServer{TokenEnv: "SUPERCARGO_TEST_MCP_TOKEN"}}
	if _, err := New(c, quiet()); err == nil || !strings.Contains(err.Error(), "SUPERCARGO_TEST_MCP_TOKEN") {
		t.Errorf("a short token: %v", err)
	}
	// From a file: its owner's alone.
	f := filepath.Join(t.TempDir(), "mcp-token")
	if err := os.WriteFile(f, []byte(testMCPToken+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.MCP.Server = &config.MCPServer{TokenFile: f}
	if _, err := New(c, quiet()); err == nil || !strings.Contains(err.Error(), "readable by others") {
		t.Errorf("a token file others can read: %v", err)
	}
	_ = os.Chmod(f, 0o600)
	a, err = New(c, quiet())
	if err != nil {
		t.Fatalf("a token file: %v", err)
	}
	rec = httptest.NewRecorder()
	a.Server.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}")))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("/mcp without the token: HTTP %d", rec.Code)
	}
	c.MCP.Server = &config.MCPServer{TokenFile: f + ".missing"}
	if _, err := New(c, quiet()); err == nil {
		t.Error("a missing token file started")
	}
}
