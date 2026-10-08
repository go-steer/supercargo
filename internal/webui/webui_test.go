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
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// echo is a control API that answers with what it was asked.
type echo struct{ got *http.Request }

func (e *echo) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.got = r
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"path": r.URL.Path, "query": r.URL.RawQuery, "cookie": r.Header.Get("Cookie")})
}

var personas = []Persona{
	{ID: "alex", Name: "Alex", Title: "Logistics", Roles: []string{RoleShipper}},
	{ID: "sam", Name: "Sam", Roles: []string{RoleOperator}},
	{ID: "jo", Name: "Jo", Roles: []string{RoleOperator, RoleShipper}},
}

func newUI(t *testing.T) (http.Handler, *echo, *time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	ctl := &echo{}
	return Handler(Config{Company: "Ocean Blue", Domain: "ocean-blue.example", Personas: personas, Control: ctl,
		Hosts: []string{"8441-ws.cloudworkstations.dev"}, Now: func() time.Time { return now }}), ctl, &now
}

func do(h http.Handler, method, path, body string, cookie *http.Cookie, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = "ocean-blue.localhost:8441"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i] == "Host" {
			req.Host = hdr[i+1]
			continue
		}
		req.Header.Set(hdr[i], hdr[i+1])
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func login(t *testing.T, h http.Handler, persona string) *http.Cookie {
	t.Helper()
	rec := do(h, http.MethodPost, "/ui/api/login", `{"persona":"`+persona+`"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login %s: HTTP %d %s", persona, rec.Code, rec.Body)
	}
	cs := rec.Result().Cookies()
	if len(cs) != 1 {
		t.Fatalf("login set %d cookies", len(cs))
	}
	return cs[0]
}

func TestStaticAndHeaders(t *testing.T) {
	h, _, _ := newUI(t)
	rec := do(h, http.MethodGet, "/ui/", "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "static/app.js") ||
		!strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Errorf("index: HTTP %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe-inline") || !strings.Contains(csp, "frame-ancestors 'self'") ||
		rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("headers: %v", rec.Header())
	}
	for path, ctype := range map[string]string{"/ui/static/app.js": "javascript", "/ui/static/app.css": "text/css"} {
		if rec := do(h, http.MethodGet, path, "", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), ctype) {
			t.Errorf("%s: HTTP %d %s", path, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
	if rec := do(h, http.MethodGet, "/ui/static/../webui.go", "", nil); rec.Code == http.StatusOK {
		t.Error("a path outside the static files was served")
	}
	var p PersonasResponse
	if rec := do(h, http.MethodGet, "/ui/api/personas", "", nil); rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &p) != nil ||
		len(p.Personas) != 3 || p.Company != "Ocean Blue" || !p.Demo || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("personas: HTTP %d %+v", rec.Code, p)
	}
}

// TestSignInRequired: nothing behind the API answers without a session.
func TestSignInRequired(t *testing.T) {
	h, ctl, _ := newUI(t)
	for _, path := range []string{"/ui/api/me", "/ui/api/messages", "/ui/api/messages/1", "/ui/api/events", "/ui/api/shipments",
		"/ui/api/bookings"} {
		if rec := do(h, http.MethodGet, path, "", &http.Cookie{Name: "ulp_session_ocean_blue_example", Value: "forged"}); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: HTTP %d", path, rec.Code)
		}
	}
	if ctl.got != nil {
		t.Error("the control API was reached without a session")
	}
}

func TestLogin(t *testing.T) {
	h, _, _ := newUI(t)
	c := login(t, h, "alex")
	if c.Name != "ulp_session_ocean_blue_example" || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/ui/" || len(c.Value) < 40 {
		t.Errorf("cookie %+v", c)
	}
	var me MeResponse
	if rec := do(h, http.MethodGet, "/ui/api/me", "", c); rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &me) != nil ||
		me.Persona.ID != "alex" || !slices.Equal(me.Sides, []string{"customer"}) {
		t.Errorf("me: HTTP %d %+v", rec.Code, me)
	}
	for name, tc := range map[string]struct {
		body string
		hdr  []string
		want int
	}{
		"unknown persona":   {`{"persona":"mallory"}`, nil, http.StatusForbidden},
		"another site":      {`{"persona":"alex"}`, []string{"Origin", "https://evil.example"}, http.StatusForbidden},
		"a form, not JSON":  {`persona=alex`, []string{"Content-Type", "application/x-www-form-urlencoded"}, http.StatusForbidden},
		"an unknown member": {`{"persona":"alex","admin":true}`, nil, http.StatusBadRequest},
	} {
		if rec := do(h, http.MethodPost, "/ui/api/login", tc.body, nil, tc.hdr...); rec.Code != tc.want || len(rec.Result().Cookies()) != 0 {
			t.Errorf("%s: HTTP %d, %d cookies", name, rec.Code, len(rec.Result().Cookies()))
		}
	}
	// The same origin, said explicitly, is fine; so is a proxy's https
	// name for it, forwarded as plain http to localhost.
	if rec := do(h, http.MethodPost, "/ui/api/login", `{"persona":"sam"}`, nil, "Origin", "http://ocean-blue.localhost:8441"); rec.Code != http.StatusOK {
		t.Errorf("same origin: HTTP %d", rec.Code)
	}
	if rec := do(h, http.MethodPost, "/ui/api/login", `{"persona":"sam"}`, nil, "Host", "localhost:8441",
		"Origin", "https://8441-ws.cloudworkstations.dev"); rec.Code != http.StatusOK {
		t.Errorf("through the workstation proxy: HTTP %d", rec.Code)
	}
	for name, origin := range map[string]string{"another scheme": "file://ocean-blue.localhost", "null": "null", "another port's host": "https://evil.example:8441",
		"the other node's UI": "http://cymbal-shoes.localhost:8442", "this host, another port": "http://ocean-blue.localhost:9999",
		"the proxy name with a port": "https://8441-ws.cloudworkstations.dev:8443"} {
		if rec := do(h, http.MethodPost, "/ui/api/login", `{"persona":"sam"}`, nil, "Origin", origin); rec.Code != http.StatusForbidden {
			t.Errorf("%s: HTTP %d", name, rec.Code)
		}
	}
}

// TestSidesForced: whatever the page asks for, the control API sees the
// persona's sides, and never the browser's cookie.
func TestSidesForced(t *testing.T) {
	h, ctl, _ := newUI(t)
	c := login(t, h, "alex")
	var got map[string]string
	rec := do(h, http.MethodGet, "/ui/api/messages?side=operator&since=3&side=customer", "", c)
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil || got["path"] != "/messages" ||
		got["query"] != "side=customer&since=3" || got["cookie"] != "" {
		t.Errorf("messages: HTTP %d %v", rec.Code, got)
	}
	do(h, http.MethodGet, "/ui/api/events?side=operator", "", c)
	if ctl.got.URL.Path != "/events" || !slices.Equal(ctl.got.URL.Query()["side"], []string{"customer"}) {
		t.Errorf("events: %v", ctl.got.URL)
	}
	jo := login(t, h, "jo")
	do(h, http.MethodGet, "/ui/api/messages/7", "", jo)
	if ctl.got.URL.Path != "/messages/7" || !slices.Equal(ctl.got.URL.Query()["side"], []string{"operator", "customer"}) {
		t.Errorf("a two-role persona: %v", ctl.got.URL)
	}
	do(h, http.MethodGet, "/ui/api/shipments/ocean-blue.example/OB%2F1/events", "", c)
	if ctl.got.URL.EscapedPath() != "/shipments/ocean-blue.example/OB%2F1/events" {
		t.Errorf("path values: %s", ctl.got.URL.EscapedPath())
	}
}

// TestRoleGates: a persona reaches only its roles' views.
func TestRoleGates(t *testing.T) {
	h, ctl, _ := newUI(t)
	alex, sam, jo := login(t, h, "alex"), login(t, h, "sam"), login(t, h, "jo")
	for _, tc := range []struct {
		who  *http.Cookie
		path string
		want int
	}{
		{alex, "/ui/api/shipments", http.StatusOK},
		{alex, "/ui/api/profile/ocean-blue.example", http.StatusNotFound}, // no UI route makes the node fetch a profile yet
		{alex, "/ui/api/bookings", http.StatusForbidden},
		{alex, "/ui/api/bookings/OB-1/events", http.StatusForbidden},
		{sam, "/ui/api/bookings", http.StatusOK},
		{sam, "/ui/api/bookings/OB-1/journey", http.StatusOK},
		{sam, "/ui/api/shipments/ocean-blue.example/OB-1/journey", http.StatusForbidden},
		{alex, "/ui/api/shipments/ocean-blue.example/OB-1/journey", http.StatusOK},
		{alex, "/ui/api/bookings/OB-1/journey", http.StatusForbidden},
		{sam, "/ui/api/shipments", http.StatusForbidden},
		{jo, "/ui/api/bookings", http.StatusOK},
		{jo, "/ui/api/shipments", http.StatusOK},
	} {
		ctl.got = nil
		rec := do(h, http.MethodGet, tc.path, "", tc.who)
		if rec.Code != tc.want || (tc.want == http.StatusOK) != (ctl.got != nil) {
			t.Errorf("%s: HTTP %d, want %d (control reached: %v)", tc.path, rec.Code, tc.want, ctl.got != nil)
		}
	}
}

func TestSessionsEnd(t *testing.T) {
	h, _, now := newUI(t)
	c := login(t, h, "alex")
	if rec := do(h, http.MethodPost, "/ui/api/logout", "{}", c, "Origin", "https://evil.example"); rec.Code != http.StatusForbidden {
		t.Errorf("a cross-site logout: HTTP %d", rec.Code)
	}
	if rec := do(h, http.MethodPost, "/ui/api/logout", "{}", c); rec.Code != http.StatusOK {
		t.Errorf("logout: HTTP %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/ui/api/me", "", c); rec.Code != http.StatusUnauthorized {
		t.Errorf("after logout: HTTP %d", rec.Code)
	}
	c = login(t, h, "alex")
	*now = now.Add(SessionTTL)
	if rec := do(h, http.MethodGet, "/ui/api/me", "", c); rec.Code != http.StatusUnauthorized {
		t.Errorf("after the TTL: HTTP %d", rec.Code)
	}
	// Sign-in has no password: sessions are capped, oldest out.
	first := login(t, h, "alex")
	for range MaxSessions {
		login(t, h, "sam")
	}
	if rec := do(h, http.MethodGet, "/ui/api/me", "", first); rec.Code != http.StatusUnauthorized {
		t.Errorf("the oldest session outlived the cap: HTTP %d", rec.Code)
	}
}

func TestPersonaSides(t *testing.T) {
	for roles, want := range map[string]string{
		"operator":                  "operator",
		"shipper":                   "customer",
		"receiver":                  "customer",
		"shipper,receiver":          "customer",
		"receiver,operator":         "customer,operator",
		"operator,shipper,receiver": "operator,customer",
	} {
		if got := strings.Join(Persona{Roles: strings.Split(roles, ",")}.Sides(), ","); got != want {
			t.Errorf("%s: %s, want %s", roles, got, want)
		}
	}
}

// TestHostCheck: the UI answers only to the names it is served as, so a
// page that rebinds its own name to this machine gets nothing, not even
// the sign-in page.
func TestHostCheck(t *testing.T) {
	h, ctl, _ := newUI(t)
	for _, host := range []string{"localhost:8441", "127.0.0.1:8441", "[::1]:8441", "cymbal-shoes.localhost", "OCEAN-BLUE.LOCALHOST.", "8441-ws.cloudworkstations.dev"} {
		if rec := do(h, http.MethodGet, "/ui/api/personas", "", nil, "Host", host); rec.Code != http.StatusOK {
			t.Errorf("%s: HTTP %d", host, rec.Code)
		}
	}
	for _, host := range []string{"evil.example:8441", "evil.example", "localhost.evil.example", "8442-ws.cloudworkstations.dev", "10.0.0.5:8441", ""} {
		for _, path := range []string{"/ui/", "/ui/api/personas"} {
			if rec := do(h, http.MethodGet, path, "", nil, "Host", host); rec.Code != http.StatusMisdirectedRequest {
				t.Errorf("%s %s: HTTP %d", host, path, rec.Code)
			}
		}
		if rec := do(h, http.MethodPost, "/ui/api/login", `{"persona":"alex"}`, nil, "Host", host, "Origin", "http://"+host); rec.Code != http.StatusMisdirectedRequest {
			t.Errorf("a rebound login via %s: HTTP %d", host, rec.Code)
		}
	}
	if ctl.got != nil {
		t.Error("the control API was reached through a foreign host")
	}
}

// TestStreamsEndWithSession: a live stream stops once its session ends,
// and a session has at most MaxStreams.
func TestStreamsEndWithSession(t *testing.T) {
	StreamCheck = 10 * time.Millisecond
	defer func() { StreamCheck = 5 * time.Second }()
	block := make(chan struct{})
	ctl := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		select {
		case <-r.Context().Done():
		case <-block:
		}
	})
	h := Handler(Config{Domain: "ocean-blue.example", Personas: personas, Control: ctl})
	c := login(t, h, "alex")
	done := make(chan int, 1)
	go func() { done <- do(h, http.MethodGet, "/ui/api/events", "", c).Code }()
	time.Sleep(50 * time.Millisecond)
	do(h, http.MethodPost, "/ui/api/logout", "{}", c)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		close(block)
		t.Fatal("the stream outlived its session")
	}
	c = login(t, h, "alex")
	var wg sync.WaitGroup
	for range MaxStreams {
		wg.Add(1)
		go func() { defer wg.Done(); do(h, http.MethodGet, "/ui/api/events", "", c) }()
	}
	time.Sleep(50 * time.Millisecond)
	if rec := do(h, http.MethodGet, "/ui/api/events", "", c); rec.Code != http.StatusTooManyRequests {
		t.Errorf("one stream too many: HTTP %d", rec.Code)
	}
	close(block)
	wg.Wait()
}

// TestForwardRefuses: no sides means no filter, so a persona with no role
// gets nothing; "." and ".." never reach the control API as path values.
func TestForwardRefuses(t *testing.T) {
	ctl := &echo{}
	h := Handler(Config{Domain: "ocean-blue.example", Personas: []Persona{{ID: "none", Name: "None"}, personas[0]}, Control: ctl})
	if rec := do(h, http.MethodGet, "/ui/api/messages", "", login(t, h, "none")); rec.Code != http.StatusForbidden {
		t.Errorf("a persona with no role: HTTP %d", rec.Code)
	}
	c := login(t, h, "alex")
	for _, path := range []string{"/ui/api/messages/..", "/ui/api/shipments/./OB-1/events", "/ui/api/shipments/x.example/../events"} {
		ctl.got = nil
		if rec := do(h, http.MethodGet, path, "", c); rec.Code == http.StatusOK && ctl.got != nil {
			t.Errorf("%s reached the control API at %s", path, ctl.got.URL)
		}
	}
	if rec := do(h, http.MethodGet, "/ui/static/", "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("/ui/static/: HTTP %d", rec.Code)
	}
}

// TestSessionReplacement: signing in again ends the old session, and the
// cap pushes out expired sessions before live ones.
func TestSessionReplacement(t *testing.T) {
	h, _, now := newUI(t)
	old := login(t, h, "alex")
	rec := do(h, http.MethodPost, "/ui/api/login", `{"persona":"sam"}`, old)
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Code)
	}
	if rec := do(h, http.MethodGet, "/ui/api/me", "", old); rec.Code != http.StatusUnauthorized {
		t.Errorf("the replaced session still works: HTTP %d", rec.Code)
	}
	for range MaxSessions - 1 {
		login(t, h, "sam")
	}
	*now = now.Add(SessionTTL)
	live := login(t, h, "alex") // the cap is passed: the expired go, not this one
	for range 5 {
		login(t, h, "sam")
	}
	if rec := do(h, http.MethodGet, "/ui/api/me", "", live); rec.Code != http.StatusOK {
		t.Errorf("a live session was pushed out while expired ones were kept: HTTP %d", rec.Code)
	}
}

// TestPresenter: the presenter page frames exactly the configured UIs
// (its CSP allows no other frame), and a UI may be framed only by its own
// pages and the configured ancestors.
func TestPresenter(t *testing.T) {
	plain, _, _ := newUI(t)
	if rec := do(plain, http.MethodGet, "/ui/presenter", "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("a node without a presenter served one: HTTP %d", rec.Code)
	}
	if csp := do(plain, http.MethodGet, "/ui/", "", nil).Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'self';") {
		t.Errorf("default frame-ancestors: %s", csp)
	}
	h := Handler(Config{Domain: "ocean-blue.example", Personas: personas, Control: &echo{},
		FrameAncestors: []string{"https://8441-ws.example"},
		Panels:         []Panel{{Name: "Cymbal Shoes", URL: "https://8442-ws.example/ui/"}, {Name: "Ocean Blue", URL: "https://8441-ws.example/ui/"}}})
	rec := do(h, http.MethodGet, "/ui/presenter", "", nil)
	csp := rec.Header().Get("Content-Security-Policy")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "presenter.js") ||
		!strings.Contains(csp, "frame-src https://8442-ws.example https://8441-ws.example;") || strings.Contains(csp, "frame-src *") {
		t.Errorf("presenter: HTTP %d, CSP %s", rec.Code, csp)
	}
	var pr PresenterResponse
	if rec := do(h, http.MethodGet, "/ui/api/presenter", "", nil); rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &pr) != nil || len(pr.Panels) != 2 {
		t.Errorf("panels: HTTP %d %+v", rec.Code, pr)
	}
	if csp := do(h, http.MethodGet, "/ui/", "", nil).Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'self' https://8441-ws.example;") {
		t.Errorf("frame-ancestors with the presenter: %s", csp)
	}
	if rec := do(h, http.MethodGet, "/ui/presenter", "", nil, "Host", "evil.example"); rec.Code != http.StatusMisdirectedRequest {
		t.Errorf("the presenter through a foreign host: HTTP %d", rec.Code)
	}
}
