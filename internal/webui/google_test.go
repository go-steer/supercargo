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
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"cloud.google.com/go/auth/credentials/idtoken"
	"golang.org/x/oauth2"

	"github.com/go-steer/supercargo/internal/googleauth"
)

const clientID = "123-abc.apps.googleusercontent.com"

// newGoogleUI is a UI with Google sign-in against a fake Google: the code
// "the-code" signs in as *email.
func newGoogleUI(t *testing.T, email *string) http.Handler {
	t.Helper()
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("code") != "the-code" || r.Form.Get("code_verifier") == "" ||
			r.Form.Get("redirect_uri") != "https://ocean-blue.example/ui/auth/callback" {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// The nonce travels in the ID token; the fake echoes it back from the code's sign-in.
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "id_token": "id-token"})
	}))
	t.Cleanup(tokens.Close)
	var nonce string
	client := &googleauth.Client{ID: clientID, Secret: "s", Endpoint: oauth2.Endpoint{AuthURL: "https://accounts.example/auth", TokenURL: tokens.URL},
		ValidateID: func(_ context.Context, tok, aud string) (*idtoken.Payload, error) {
			if tok != "id-token" || aud != clientID {
				return nil, errors.New("bad")
			}
			return &idtoken.Payload{Issuer: "https://accounts.google.com", Claims: map[string]any{"email": *email, "email_verified": true, "nonce": nonce}}, nil
		}}
	members := []Persona{
		{ID: "dana", Name: "Dana", Roles: []string{RoleShipper}, Members: []string{"dana@ocean-blue.example"}},
		{ID: "ops", Name: "Ops", Roles: []string{RoleOperator}, Members: []string{"@ocean-blue.example"}},
	}
	h := Handler(Config{Company: "Ocean Blue", Domain: "ocean-blue.example", Personas: members, Control: &echo{}, Secure: true,
		Hosts: []string{"ocean-blue.example"}, Google: &Google{Client: client, PublicURL: "https://ocean-blue.example"}})
	// Catch the nonce as the browser leaves for Google.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r)
		if r.URL.Path == LoginPath {
			if u, err := url.Parse(w.Header().Get("Location")); err == nil {
				nonce = u.Query().Get("nonce")
			}
		}
	})
}

func cookieNamed(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// start begins a sign-in: Google's URL, and the sign-in cookie.
func start(t *testing.T, h http.Handler) (url.Values, *http.Cookie) {
	t.Helper()
	rec := do(h, http.MethodGet, LoginPath, "", nil, "Host", "ocean-blue.example")
	if rec.Code != http.StatusFound {
		t.Fatalf("login: %d", rec.Code)
	}
	u, _ := url.Parse(rec.Header().Get("Location"))
	c := cookieNamed(rec, "ulp_signin_ocean_blue_example")
	if u.Host != "accounts.example" || c == nil || c.SameSite != http.SameSiteLaxMode || !c.HttpOnly || !c.Secure || c.Path != "/ui/auth/" {
		t.Fatalf("login went to %s with cookie %+v", u, c)
	}
	return u.Query(), c
}

func TestGoogleSignIn(t *testing.T) {
	email := "dana@ocean-blue.example"
	h := newGoogleUI(t, &email)
	host := []string{"Host", "ocean-blue.example"}

	// No personas to pick, and no password sign-in.
	rec := do(h, http.MethodGet, "/ui/api/personas", "", nil, host...)
	var pr PersonasResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &pr); err != nil || len(pr.Personas) != 0 || pr.Demo || pr.Login != LoginPath {
		t.Errorf("personas = %s", rec.Body)
	}
	if rec := do(h, http.MethodPost, "/ui/api/login", `{"persona":"dana"}`, nil, host...); rec.Code != http.StatusForbidden || cookieNamed(rec, "ulp_session_ocean_blue_example") != nil {
		t.Errorf("persona login with Google sign-in: %d", rec.Code)
	}

	q, c := start(t, h)
	if q.Get("redirect_uri") != "https://ocean-blue.example/ui/auth/callback" {
		t.Errorf("redirect_uri %q", q.Get("redirect_uri"))
	}
	rec = do(h, http.MethodGet, CallbackPath+"?state="+url.QueryEscape(q.Get("state"))+"&code=the-code", "", c, host...)
	session := cookieNamed(rec, "ulp_session_ocean_blue_example")
	if rec.Code != http.StatusOK || session == nil || session.SameSite != http.SameSiteStrictMode || !session.Secure {
		t.Fatalf("callback: %d %s, session %+v", rec.Code, rec.Body, session)
	}
	if !strings.Contains(rec.Body.String(), `http-equiv="refresh" content="0;url=/ui/"`) || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("the landing page doesn't move on: %s", rec.Body)
	}
	if gone := cookieNamed(rec, "ulp_signin_ocean_blue_example"); gone == nil || gone.MaxAge >= 0 {
		t.Error("the sign-in cookie outlived its sign-in")
	}
	rec = do(h, http.MethodGet, "/ui/api/me", "", session, host...)
	var me MeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil || me.Persona.ID != "dana" || me.Persona.Email != email || me.Demo {
		t.Errorf("me = %s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), "members") {
		t.Error("members reached the browser")
	}

	// The first persona that has them: anyone else in the domain is Ops.
	email = "lee@ocean-blue.example"
	q, c = start(t, h)
	rec = do(h, http.MethodGet, CallbackPath+"?state="+url.QueryEscape(q.Get("state"))+"&code=the-code", "", c, host...)
	if s := cookieNamed(rec, "ulp_session_ocean_blue_example"); s == nil {
		t.Fatal("lee wasn't signed in")
	} else if err := json.Unmarshal(do(h, http.MethodGet, "/ui/api/me", "", s, host...).Body.Bytes(), &me); err != nil || me.Persona.ID != "ops" {
		t.Errorf("lee is %+v", me.Persona)
	}
}

func TestGoogleSignInRejects(t *testing.T) {
	email := "dana@ocean-blue.example"
	h := newGoogleUI(t, &email)
	host := []string{"Host", "ocean-blue.example"}
	for name, tc := range map[string]struct {
		email string
		query func(state string) string
		drop  bool // no sign-in cookie
		want  int
	}{
		"a forged state":    {query: func(string) string { return "?state=forged&code=the-code" }, want: http.StatusUnauthorized},
		"no state":          {query: func(string) string { return "?code=the-code" }, want: http.StatusUnauthorized},
		"no sign-in cookie": {query: func(s string) string { return "?state=" + url.QueryEscape(s) + "&code=the-code" }, drop: true, want: http.StatusUnauthorized},
		"a refused code":    {query: func(s string) string { return "?state=" + url.QueryEscape(s) + "&code=stolen" }, want: http.StatusUnauthorized},
		"cancelled":         {query: func(s string) string { return "?state=" + url.QueryEscape(s) + "&error=access_denied" }, want: http.StatusUnauthorized},
		"not a member": {email: "mallory@evil.example", query: func(s string) string { return "?state=" + url.QueryEscape(s) + "&code=the-code" },
			want: http.StatusForbidden},
	} {
		email = "dana@ocean-blue.example"
		if tc.email != "" {
			email = tc.email
		}
		q, c := start(t, h)
		if tc.drop {
			c = nil
		}
		rec := do(h, http.MethodGet, CallbackPath+tc.query(q.Get("state")), "", c, host...)
		if rec.Code != tc.want || cookieNamed(rec, "ulp_session_ocean_blue_example") != nil {
			t.Errorf("%s: %d, session %v; want %d and none", name, rec.Code, cookieNamed(rec, "ulp_session_ocean_blue_example"), tc.want)
		}
		if strings.Contains(rec.Body.String(), "<script") {
			t.Errorf("%s: a script in the page", name)
		}
	}
	// An email can't inject markup into the page that names it.
	email = `<b>x</b>@evil.example`
	q, c := start(t, h)
	rec := do(h, http.MethodGet, CallbackPath+"?state="+url.QueryEscape(q.Get("state"))+"&code=the-code", "", c, host...)
	if strings.Contains(rec.Body.String(), "<b>") {
		t.Errorf("unescaped: %s", rec.Body)
	}
	// Demo sign-in has no Google routes.
	demo, _, _ := newUI(t)
	if rec := do(demo, http.MethodGet, LoginPath, "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("a demo UI served %s: %d", LoginPath, rec.Code)
	}
}
