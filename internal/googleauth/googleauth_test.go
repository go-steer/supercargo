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

package googleauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/auth/credentials/idtoken"
	"golang.org/x/oauth2"
)

const clientID = "123-abc.apps.googleusercontent.com"

var now = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

// tokeninfo answers for the tokens it knows, as Google's endpoint does.
func tokeninfo(t *testing.T, tokens map[string]map[string]string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Method != http.MethodPost || r.URL.RawQuery != "" {
			t.Errorf("tokeninfo got %s %s: the token must go in the body", r.Method, r.URL)
		}
		info, ok := tokens[r.FormValue("access_token")]
		if !ok {
			http.Error(w, `{"error":"invalid_token"}`, http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(info)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func info(aud, email, verified string, exp time.Time) map[string]string {
	return map[string]string{"aud": aud, "azp": aud, "email": email, "email_verified": verified, "exp": strconv.FormatInt(exp.Unix(), 10)}
}

func TestAccessToken(t *testing.T) {
	srv, hits := tokeninfo(t, map[string]map[string]string{
		"good":       info(clientID, "Dana@Cymbal.example", "true", now.Add(time.Hour)),
		"other-app":  info("999-other.apps.googleusercontent.com", "dana@cymbal.example", "true", now.Add(time.Hour)),
		"unverified": info(clientID, "dana@cymbal.example", "false", now.Add(time.Hour)),
		"no-email":   info(clientID, "", "true", now.Add(time.Hour)),
		"expired":    info(clientID, "dana@cymbal.example", "true", now.Add(-time.Second)),
		"bad-exp":    {"aud": clientID, "email": "dana@cymbal.example", "email_verified": "true", "exp": "soon"},
	})
	clock := now
	c := &Client{ID: clientID, TokenInfo: srv.URL, Now: func() time.Time { return clock }}
	ctx := context.Background()

	id, err := c.AccessToken(ctx, "good")
	if err != nil || id.Email != "dana@cymbal.example" {
		t.Fatalf("good token: %+v, %v", id, err)
	}
	for _, tok := range []string{"", "unknown", "other-app", "unverified", "no-email", "expired", "bad-exp", string(make([]byte, 5000))} {
		if _, err := c.AccessToken(ctx, tok); !errors.Is(err, ErrInvalid) {
			t.Errorf("token %.20q: %v, want ErrInvalid", tok, err)
		}
	}

	// Cached, but for at most MaxTTL, then asked again.
	n := hits.Load()
	if _, err := c.AccessToken(ctx, "good"); err != nil || hits.Load() != n {
		t.Errorf("a checked token was checked again (%v)", err)
	}
	clock = now.Add(MaxTTL)
	if _, err := c.AccessToken(ctx, "good"); err != nil || hits.Load() != n+1 {
		t.Errorf("a token was trusted past MaxTTL (%v)", err)
	}
	// Failures are never cached as successes, and a failure isn't remembered
	// as one either: an unknown token asks Google every time.
	n = hits.Load()
	_, _ = c.AccessToken(ctx, "unknown")
	_, _ = c.AccessToken(ctx, "unknown")
	if hits.Load() != n+2 {
		t.Error("a refused token was cached")
	}
}

func TestAccessTokenCacheBound(t *testing.T) {
	tokens := map[string]map[string]string{}
	for i := range MaxCached + 10 {
		tokens["t"+strconv.Itoa(i)] = info(clientID, "dana@cymbal.example", "true", now.Add(time.Hour))
	}
	srv, _ := tokeninfo(t, tokens)
	c := &Client{ID: clientID, TokenInfo: srv.URL, Now: func() time.Time { return now }}
	for tok := range tokens {
		if _, err := c.AccessToken(context.Background(), tok); err != nil {
			t.Fatal(err)
		}
	}
	if len(c.cache) > MaxCached {
		t.Errorf("cache holds %d, want at most %d", len(c.cache), MaxCached)
	}
}

// tokenEndpoint is a token endpoint that hands out id_token for the expected
// code and verifier.
func tokenEndpoint(t *testing.T, idToken string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("code") != "the-code" || r.Form.Get("code_verifier") == "" {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 3600, "id_token": idToken})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSignIn(t *testing.T) {
	srv := tokenEndpoint(t, "the-id-token")
	const redirect = "https://cymbal.example/ui/auth/callback"
	var claims map[string]any
	iss := "https://accounts.google.com"
	c := &Client{ID: clientID, Secret: "s", Endpoint: oauth2.Endpoint{AuthURL: "https://accounts.example/auth", TokenURL: srv.URL},
		ValidateID: func(_ context.Context, tok, aud string) (*idtoken.Payload, error) {
			if tok != "the-id-token" || aud != clientID {
				return nil, errors.New("bad token")
			}
			return &idtoken.Payload{Issuer: iss, Audience: aud, Claims: claims}, nil
		}}

	u, s := c.Start(redirect)
	q := mustQuery(t, u)
	if q.Get("state") != s.State || q.Get("nonce") != s.Nonce || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" ||
		q.Get("redirect_uri") != redirect || q.Get("client_id") != clientID || q.Get("scope") != "openid email" {
		t.Errorf("auth URL %s", u)
	}
	if q.Get("code_challenge") == s.Verifier {
		t.Error("the verifier went out in the clear")
	}
	if _, s2 := c.Start(redirect); s2.State == s.State || s2.Nonce == s.Nonce || s2.Verifier == s.Verifier {
		t.Error("two sign-ins share secrets")
	}

	good := func() map[string]any {
		return map[string]any{"email": "Dana@cymbal.example", "email_verified": true, "nonce": s.Nonce}
	}
	ctx := context.Background()
	claims = good()
	id, err := c.Finish(ctx, redirect, s, s.State, "the-code")
	if err != nil || id.Email != "dana@cymbal.example" {
		t.Fatalf("sign-in: %+v, %v", id, err)
	}
	iss = "accounts.google.com"
	if _, err := c.Finish(ctx, redirect, s, s.State, "the-code"); err != nil {
		t.Errorf("the bare issuer: %v", err)
	}

	for name, tc := range map[string]struct {
		s           SignIn
		state, code string
		iss         string
		change      func(map[string]any)
	}{
		"state mismatch":   {s: s, state: "forged", code: "the-code"},
		"no state at all":  {s: SignIn{}, state: "", code: "the-code"},
		"no code":          {s: s, state: s.State, code: ""},
		"code refused":     {s: s, state: s.State, code: "other"},
		"other issuer":     {s: s, state: s.State, code: "the-code", iss: "https://evil.example"},
		"replayed nonce":   {s: s, state: s.State, code: "the-code", change: func(m map[string]any) { m["nonce"] = "old" }},
		"no nonce":         {s: s, state: s.State, code: "the-code", change: func(m map[string]any) { delete(m, "nonce") }},
		"unverified email": {s: s, state: s.State, code: "the-code", change: func(m map[string]any) { m["email_verified"] = false }},
		"string verified":  {s: s, state: s.State, code: "the-code", change: func(m map[string]any) { m["email_verified"] = "true" }},
		"no email":         {s: s, state: s.State, code: "the-code", change: func(m map[string]any) { delete(m, "email") }},
	} {
		claims = good()
		iss = "https://accounts.google.com"
		if tc.iss != "" {
			iss = tc.iss
		}
		if tc.change != nil {
			tc.change(claims)
		}
		if _, err := c.Finish(ctx, redirect, tc.s, tc.state, tc.code); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}

	// The ID token itself is refused (Google's signature, audience, expiry).
	c.ValidateID = func(context.Context, string, string) (*idtoken.Payload, error) {
		return nil, errors.New("bad signature")
	}
	claims = good()
	if _, err := c.Finish(ctx, redirect, s, s.State, "the-code"); !errors.Is(err, ErrInvalid) {
		t.Errorf("a refused ID token: %v", err)
	}
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}

func TestMember(t *testing.T) {
	members := []string{"dana@cymbal.example", "@ops.cymbal.example", " Lee@Partner.example "}
	for email, want := range map[string]bool{
		"dana@cymbal.example":         true,
		"DANA@cymbal.example":         true,
		"sam@ops.cymbal.example":      true,
		"lee@partner.example":         true,
		"sam@cymbal.example":          false,
		"sam@evil-ops.cymbal.example": false,
		"sam@xops.cymbal.example":     false,
		"@ops.cymbal.example":         false,
		"ops.cymbal.example":          false,
		"":                            false,
	} {
		if got := Member(email, members); got != want {
			t.Errorf("Member(%q) = %v, want %v", email, got, want)
		}
	}
}
