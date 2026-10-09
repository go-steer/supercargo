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
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/go-steer/supercargo/internal/activity"
	"github.com/go-steer/supercargo/internal/assistant"
	"github.com/go-steer/supercargo/internal/config"
	"github.com/go-steer/supercargo/internal/webui"
)

type bearerOf struct {
	token string
	base  http.RoundTripper
}

func (b bearerOf) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

// TestGoogleSignIn: a node with Google sign-in for its UI and its MCP
// endpoint. Only the members of a persona acting as shipper may call the
// tools, each call is logged with who made it, and the metadata points
// MCP clients at Google.
func TestGoogleSignIn(t *testing.T) {
	t.Setenv("SUPERCARGO_TEST_GOOGLE_SECRET", "a-client-secret")
	p := startPairWith(t, "", `roles: [shipper, receiver]
google: { clientID: 123-abc.apps.googleusercontent.com, clientSecretEnv: SUPERCARGO_TEST_GOOGLE_SECRET, publicURL: "https://cymbal-shoes.example" }
mcp: { server: { auth: google } }
web:
  auth: google
  personas:
    - { id: alex, name: Alex, roles: [shipper], members: [alex@cymbal-shoes.example] }
    - { id: rae, name: Rae, roles: [receiver], members: ["@cymbal-shoes.example"] }`)
	a := p.shipper
	exp := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := r.FormValue("access_token")
		if r.URL.Path == "/userinfo" {
			tok = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		email := map[string]string{"alex-token": "alex@cymbal-shoes.example", "rae-token": "rae@cymbal-shoes.example"}[tok]
		switch {
		case email == "":
			http.Error(w, "{}", http.StatusBadRequest)
		case r.URL.Path == "/userinfo":
			_ = json.NewEncoder(w).Encode(map[string]string{"email": email, "hd": "cymbal-shoes.example"})
		default:
			_ = json.NewEncoder(w).Encode(map[string]string{"aud": "123-abc.apps.googleusercontent.com", "email": email, "email_verified": "true", "exp": exp,
				"scope": "openid https://www.googleapis.com/auth/userinfo.email"})
		}
	}))
	defer google.Close()
	a.google.TokenInfo, a.google.UserInfo = google.URL+"/tokeninfo", google.URL+"/userinfo"
	srv := httptest.NewServer(a.Server.Handler)
	defer srv.Close()

	connect := func(tok string) (*mcp.ClientSession, error) {
		c := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "1"}, nil)
		return c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp",
			HTTPClient: &http.Client{Transport: bearerOf{tok, http.DefaultTransport}}, MaxRetries: -1}, nil)
	}
	for _, tok := range []string{"rae-token", "someone-elses-token", ""} {
		if cs, err := connect(tok); err == nil {
			_ = cs.Close()
			t.Errorf("%q reached the shipper's tools", tok)
		}
	}
	cs, err := connect("alex-token")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	if r, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: assistant.ToolList, Arguments: map[string]any{}}); err != nil || r.IsError {
		t.Fatalf("list over MCP: %v %+v", err, r)
	}
	past, _, _, cancel := a.Activity.Subscribe(0, 0)
	cancel()
	found := false
	for _, e := range past {
		if e.Kind == activity.KindTool && e.Tool.Caller == "alex@cymbal-shoes.example" {
			found = true
		}
	}
	if !found {
		t.Error("the call isn't in the activity with its caller")
	}

	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = "cymbal-shoes.example"
		rec := httptest.NewRecorder()
		a.Server.Handler.ServeHTTP(rec, req)
		return rec
	}
	for _, path := range []string{"/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-protected-resource"} {
		var meta struct {
			Resource string   `json:"resource"`
			Servers  []string `json:"authorization_servers"`
		}
		if rec := get(path); json.Unmarshal(rec.Body.Bytes(), &meta) != nil || meta.Resource != "https://cymbal-shoes.example/mcp" ||
			len(meta.Servers) != 1 || meta.Servers[0] != "https://accounts.google.com" {
			t.Errorf("%s: %d %s", path, rec.Code, rec.Body)
		}
	}
	// Unauthenticated, /mcp names the metadata.
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	a.Server.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Header().Get("WWW-Authenticate"), "https://cymbal-shoes.example/.well-known/oauth-protected-resource/mcp") {
		t.Errorf("/mcp without a token: %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}

	// The UI, reached at its public name, sends people to Google.
	rec = get(webui.LoginPath)
	u, _ := url.Parse(rec.Header().Get("Location"))
	if rec.Code != http.StatusFound || u.Host != "accounts.google.com" || u.Query().Get("redirect_uri") != "https://cymbal-shoes.example/ui/auth/callback" ||
		u.Query().Get("client_id") != "123-abc.apps.googleusercontent.com" {
		t.Errorf("login: %d %s", rec.Code, u)
	}
	if c := rec.Result().Cookies(); len(c) != 1 || !c[0].Secure {
		t.Errorf("the sign-in cookie: %+v", c)
	}
}

// TestGoogleSecret: the client secret from a file must be its owner's
// alone, and must be there.
func TestGoogleSecret(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "secret")
	if err := os.WriteFile(file, []byte("s3cret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := secretFrom("", file); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("a world-readable secret: %v", err)
	}
	_ = os.Chmod(file, 0o600)
	if s, from, err := secretFrom("", file); err != nil || s != "s3cret" || from != file {
		t.Errorf("secret: %q %q %v", s, from, err)
	}
	if _, _, err := secretFrom("", filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing secret file: no error")
	}
}

// TestProxyMode: a node behind a proxy that serves https at its domain
// (Cloud Run): plain HTTP on its listener, its key from the environment
// (and then gone from it), https everywhere it publishes.
func TestProxyMode(t *testing.T) {
	pemData, err := os.ReadFile(keyFile(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUPERCARGO_TEST_SIGNING_KEY", string(pemData))
	cfg, err := config.Parse([]byte(`domain: ocean-blue-123.us-central1.run.app
name: Ocean Blue
role: operator
listen: ":8080"
tls: { mode: proxy }
keys: [{ kid: ob-1, privateKeyEnv: SUPERCARGO_TEST_SIGNING_KEY }]
operator: { refPrefix: OB-BKG, containerPrefix: OCBU, vessel: Aurora, voyage: 42W, departureHour: 18, transit: 134h }
`))
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(cfg, quiet())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := os.LookupEnv("SUPERCARGO_TEST_SIGNING_KEY"); ok {
		t.Error("the signing key stayed in the environment")
	}
	if got := RESTURL(cfg); got != "https://ocean-blue-123.us-central1.run.app/ulp" {
		t.Errorf("RESTURL = %s", got)
	}
	srv := httptest.NewServer(a.Server.Handler)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/.well-known/ulp")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "https://ocean-blue-123.us-central1.run.app/ulp") {
		t.Errorf("profile: %d %s", resp.StatusCode, body)
	}
}
