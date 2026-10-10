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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/auth/credentials/idtoken"
	"golang.org/x/oauth2"
)

const clientID = "123-abc.apps.googleusercontent.com"

var now = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

// fakeGoogle answers tokeninfo (POST /tokeninfo) and userinfo (GET
// /userinfo) for the tokens it knows, as Google's endpoints do. A token's
// "hd" and "user_email" go in userinfo's answer, the rest in tokeninfo's;
// "status" makes both fail with that HTTP status.
func fakeGoogle(t *testing.T, tokens map[string]map[string]string) (*Client, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		var tok string
		switch r.URL.Path {
		case "/tokeninfo":
			if r.Method != http.MethodPost || r.URL.RawQuery != "" {
				t.Errorf("tokeninfo got %s %s: the token must go in the body", r.Method, r.URL)
			}
			tok = r.FormValue("access_token")
		case "/userinfo":
			tok = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		info, ok := tokens[tok]
		if !ok {
			http.Error(w, `{"error":"invalid_token"}`, http.StatusBadRequest)
			return
		}
		if s := info["status"]; s != "" {
			code, _ := strconv.Atoi(s)
			http.Error(w, "{}", code)
			return
		}
		out := map[string]string{}
		for k, v := range info {
			switch {
			case r.URL.Path == "/userinfo" && k == "hd":
				out["hd"] = v
			case r.URL.Path == "/userinfo" && k == "user_email":
				out["email"] = v
			case r.URL.Path == "/tokeninfo" && k != "hd" && k != "user_email":
				out[k] = v
			}
		}
		if _, ok := out["email"]; !ok && r.URL.Path == "/userinfo" {
			out["email"] = info["email"]
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return &Client{ID: clientID, TokenInfo: srv.URL + "/tokeninfo", UserInfo: srv.URL + "/userinfo", Now: func() time.Time { return now }}, &hits
}

func info(aud, email, verified string, exp time.Time) map[string]string {
	return map[string]string{"aud": aud, "azp": aud, "email": email, "email_verified": verified, "exp": strconv.FormatInt(exp.Unix(), 10),
		"scope": "openid " + EmailScope, "hd": "cymbal.example"}
}

func with(m map[string]string, kv ...string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == "" {
			delete(out, kv[i])
		} else {
			out[kv[i]] = kv[i+1]
		}
	}
	return out
}

func TestAccessToken(t *testing.T) {
	good := info(clientID, "Dana@Cymbal.example", "true", now.Add(time.Hour))
	c, hits := fakeGoogle(t, map[string]map[string]string{
		"good":          good,
		"consumer":      with(good, "hd", ""),
		"other-app":     with(good, "aud", "999-other.apps.googleusercontent.com", "azp", "999-other.apps.googleusercontent.com"),
		"no-scope":      with(good, "scope", "openid"),
		"unverified":    with(good, "email_verified", "false"),
		"no-email":      with(good, "email", ""),
		"expired":       with(good, "exp", strconv.FormatInt(now.Add(-time.Second).Unix(), 10)),
		"bad-exp":       with(good, "exp", "soon"),
		"other-user":    with(good, "user_email", "mallory@cymbal.example"),
		"google-down":   with(good, "status", "503"),
		"google-busy":   with(good, "status", "429"),
		"eyJh.eyJi.sig": good, // a JWT: never even asked about
	})
	clock := now
	c.Now = func() time.Time { return clock }
	ctx := context.Background()

	id, err := c.AccessToken(ctx, "good")
	if err != nil || id.Email != "dana@cymbal.example" || id.HD != "cymbal.example" {
		t.Fatalf("good token: %+v, %v", id, err)
	}
	if id, err := c.AccessToken(ctx, "consumer"); err != nil || id.HD != "" {
		t.Errorf("a consumer account: %+v, %v (accepted, with no domain)", id, err)
	}
	for _, tok := range []string{"", "unknown", "other-app", "no-scope", "unverified", "no-email", "expired", "bad-exp", "other-user",
		"eyJh.eyJi.sig", string(make([]byte, 5000))} {
		if _, err := c.AccessToken(ctx, tok); !errors.Is(err, ErrInvalid) {
			t.Errorf("token %.20q: %v, want ErrInvalid", tok, err)
		}
	}
	// Google failing isn't Google refusing: not ErrInvalid, and not
	// remembered.
	for _, tok := range []string{"google-down", "google-busy"} {
		n := hits.Load()
		for range 2 {
			if _, err := c.AccessToken(ctx, tok); err == nil || errors.Is(err, ErrInvalid) {
				t.Errorf("%s: %v, want a failure that isn't a refusal", tok, err)
			}
		}
		if hits.Load() != n+2 {
			t.Errorf("%s: a failure was remembered", tok)
		}
	}

	// Cached, but for at most MaxTTL, then asked again.
	n := hits.Load()
	if _, err := c.AccessToken(ctx, "good"); err != nil || hits.Load() != n {
		t.Errorf("a checked token was checked again (%v)", err)
	}
	clock = now.Add(MaxTTL)
	if _, err := c.AccessToken(ctx, "good"); err != nil || hits.Load() != n+2 {
		t.Errorf("a token was trusted past MaxTTL (%v)", err)
	}
	// A refusal is remembered for RefusedTTL, and never as a success.
	n = hits.Load()
	_, _ = c.AccessToken(ctx, "unknown-2")
	if _, err := c.AccessToken(ctx, "unknown-2"); !errors.Is(err, ErrInvalid) || hits.Load() != n+1 {
		t.Errorf("a refused token: %v, %d lookups", err, hits.Load()-n)
	}
	clock = clock.Add(RefusedTTL)
	_, _ = c.AccessToken(ctx, "unknown-2")
	if hits.Load() != n+2 {
		t.Error("a refusal was remembered past RefusedTTL")
	}
	// The JWT never reached Google.
	if _, err := c.AccessToken(ctx, "eyJh.eyJi.sig"); !errors.Is(err, ErrInvalid) {
		t.Error("a JWT passed")
	}
}

// TestAccessTokenLookups: past MaxLookups at once, a new token is refused
// without asking Google.
func TestAccessTokenLookups(t *testing.T) {
	release := make(chan struct{})
	var inflight atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		inflight.Add(1)
		<-release
		http.Error(w, "{}", http.StatusBadRequest)
	}))
	defer srv.Close()
	c := &Client{ID: clientID, TokenInfo: srv.URL, UserInfo: srv.URL}
	done := make(chan struct{})
	for i := range MaxLookups {
		go func() { _, _ = c.AccessToken(context.Background(), "slow-"+strconv.Itoa(i)); done <- struct{}{} }()
	}
	for inflight.Load() < MaxLookups {
		time.Sleep(time.Millisecond)
	}
	if _, err := c.AccessToken(context.Background(), "one-more"); !errors.Is(err, ErrInvalid) || inflight.Load() != MaxLookups {
		t.Errorf("past MaxLookups: %v, %d lookups", err, inflight.Load())
	}
	close(release)
	for range MaxLookups {
		<-done
	}
}

func TestAccessTokenCacheBound(t *testing.T) {
	tokens := map[string]map[string]string{}
	for i := range MaxCached + 10 {
		tokens["t"+strconv.Itoa(i)] = info(clientID, "dana@cymbal.example", "true", now.Add(time.Hour))
	}
	c, _ := fakeGoogle(t, tokens)
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
		return map[string]any{"email": "Dana@cymbal.example", "email_verified": true, "nonce": s.Nonce, "hd": "Cymbal.example"}
	}
	ctx := context.Background()
	claims = good()
	id, err := c.Finish(ctx, redirect, s, s.State, "the-code")
	if err != nil || id.Email != "dana@cymbal.example" || id.HD != "cymbal.example" {
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
	for _, tc := range []struct {
		email, hd string
		want      bool
	}{
		{"dana@cymbal.example", "", true}, // an exact address needs no domain
		{"DANA@cymbal.example", "cymbal.example", true},
		{"sam@ops.cymbal.example", "ops.cymbal.example", true},
		{"SAM@ops.cymbal.example", "OPS.cymbal.example", true},
		{"lee@partner.example", "", true},
		// A domain entry needs the Workspace's word: a consumer account
		// can carry the address.
		{"sam@ops.cymbal.example", "", false},
		{"sam@ops.cymbal.example", "evil.example", false},
		{"sam@ops.cymbal.example", "cymbal.example", false},
		{"sam@a.ops.cymbal.example", "a.ops.cymbal.example", false},
		{"sam@cymbal.example", "cymbal.example", false},
		{"sam@evil-ops.cymbal.example", "evil-ops.cymbal.example", false},
		{"sam@xops.cymbal.example", "xops.cymbal.example", false},
		{"@ops.cymbal.example", "ops.cymbal.example", false},
		{"ops.cymbal.example", "ops.cymbal.example", false},
		{"", "", false},
	} {
		if got := (Identity{Email: tc.email, HD: tc.hd}).Member(members); got != tc.want {
			t.Errorf("%s (hd %q): member = %v, want %v", tc.email, tc.hd, got, tc.want)
		}
	}
}
