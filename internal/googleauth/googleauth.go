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

// Package googleauth signs people in with Google, for a hosted node (#39):
// one OAuth client of the company's own, used two ways.
//
//   - Access tokens, presented by an MCP client signed in as a person
//     (Gemini Enterprise sends the user's token): checked with Google's
//     tokeninfo endpoint (issued to our client, not expired, the email
//     scope, a verified email), then its userinfo endpoint (the account's
//     Workspace domain), and the result cached for at most MaxTTL.
//   - The browser UI's sign-in: the authorization-code flow with PKCE and a
//     nonce, server-side, whose ID token is verified (Google's signature,
//     issuer, our client as the audience, the nonce, a verified email).
//
// Either way the outcome is an email and the account's Workspace domain
// (hd), which the caller maps to the roles it allows with Member; this
// package decides nothing about access.
package googleauth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/auth/credentials/idtoken"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// Google's endpoints.
const (
	TokenInfoURL = "https://oauth2.googleapis.com/tokeninfo" //nolint:gosec // G101: an endpoint, not a credential
	UserInfoURL  = "https://openidconnect.googleapis.com/v1/userinfo"
	// EmailScope is the scope an access token must carry.
	EmailScope = "https://www.googleapis.com/auth/userinfo.email"
	// Issuer is the authorization server MCP clients are pointed at.
	Issuer = "https://accounts.google.com"
)

// Scopes the sign-ins ask for: who the person is, nothing else.
var Scopes = []string{"openid", "email"}

// Limits.
const (
	// MaxCached bounds the access tokens remembered.
	MaxCached = 1024
	// MaxTTL bounds how long a checked token is trusted without asking
	// Google again, whatever its own expiry.
	MaxTTL = 10 * time.Minute
	// RefusedTTL is how long a refused token is refused without asking
	// Google again, and MaxLookups how many lookups run at once (past
	// that, a new token is refused): garbage tokens can't turn into a
	// flood of calls to Google.
	RefusedTTL = 30 * time.Second
	MaxLookups = 8
)

// ErrInvalid is a token or a sign-in that doesn't hold.
var ErrInvalid = errors.New("googleauth: invalid")

// Identity is who a token or a sign-in belongs to.
type Identity struct {
	Email string
	// HD is the account's Workspace domain, as Google asserts it; empty
	// for a consumer account, whatever its email.
	HD string
}

// Client is the company's OAuth client.
type Client struct {
	ID, Secret string
	// HTTP fetches tokeninfo and exchanges codes (default: 10 s timeout).
	HTTP *http.Client
	// TokenInfo, UserInfo and Endpoint replace Google's (tests).
	TokenInfo, UserInfo string
	Endpoint            oauth2.Endpoint
	// ValidateID replaces Google's ID token check (tests).
	ValidateID func(ctx context.Context, token, audience string) (*idtoken.Payload, error)
	Now        func() time.Time

	mu      sync.Mutex
	cache   map[string]cached
	lookups int
}

type cached struct {
	id      Identity
	expires time.Time
	refused bool
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// AccessToken checks an access token presented as a bearer token: issued
// to this client, unexpired, with the email scope, for a verified email.
func (c *Client) AccessToken(ctx context.Context, token string) (Identity, error) {
	// A JWT is an ID token, never an access token: refused before Google
	// is asked, so nothing depends on tokeninfo telling them apart.
	if token == "" || len(token) > 4096 || strings.HasPrefix(token, "eyJ") && strings.Count(token, ".") == 2 {
		return Identity{}, ErrInvalid
	}
	sum := sha256.Sum256([]byte(token))
	key := hex.EncodeToString(sum[:])
	now := c.now()
	c.mu.Lock()
	if e, ok := c.cache[key]; ok && now.Before(e.expires) {
		c.mu.Unlock()
		if e.refused {
			return Identity{}, ErrInvalid
		}
		return e.id, nil
	}
	if c.lookups >= MaxLookups {
		c.mu.Unlock()
		return Identity{}, fmt.Errorf("%w: too many sign-ins at once", ErrInvalid)
	}
	c.lookups++
	c.mu.Unlock()
	id, until, err := c.lookup(ctx, token, now)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lookups--
	switch {
	case err == nil:
		c.remember(key, cached{id: id, expires: until}, now)
	case errors.Is(err, ErrInvalid):
		// Google said no: say no again for a while without asking.
		c.remember(key, cached{refused: true, expires: now.Add(RefusedTTL)}, now)
	}
	return id, err
}

// remember caches e under key, within MaxCached. Call with mu held.
func (c *Client) remember(key string, e cached, now time.Time) {
	if c.cache == nil {
		c.cache = map[string]cached{}
	}
	if len(c.cache) >= MaxCached {
		for k, old := range c.cache {
			if !now.Before(old.expires) || len(c.cache) >= MaxCached {
				delete(c.cache, k)
			}
		}
	}
	c.cache[key] = e
}

// lookup asks Google about token: tokeninfo for whom it was issued to
// and what it may do, userinfo for the account's domain.
func (c *Client) lookup(ctx context.Context, token string, now time.Time) (Identity, time.Time, error) {
	endpoint := c.TokenInfo
	if endpoint == "" {
		endpoint = TokenInfoURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(url.Values{"access_token": {token}}.Encode()))
	if err != nil {
		return Identity{}, time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var info struct {
		Aud           string `json:"aud"`
		Azp           string `json:"azp"`
		Scope         string `json:"scope"`
		Email         string `json:"email"`
		EmailVerified string `json:"email_verified"`
		Exp           string `json:"exp"`
	}
	if err := c.fetch(req, "tokeninfo", &info); err != nil {
		return Identity{}, time.Time{}, err
	}
	exp, err := strconv.ParseInt(info.Exp, 10, 64)
	switch {
	case err != nil, !time.Unix(exp, 0).After(now):
		return Identity{}, time.Time{}, ErrInvalid
	case info.Aud != c.ID && info.Azp != c.ID:
		// Issued to someone else's client: never accepted here.
		return Identity{}, time.Time{}, ErrInvalid
	case !slices.Contains(strings.Fields(info.Scope), EmailScope):
		return Identity{}, time.Time{}, ErrInvalid
	case info.Email == "" || info.EmailVerified != "true":
		return Identity{}, time.Time{}, ErrInvalid
	}

	// The domain: tokeninfo doesn't say, and an email alone doesn't show
	// that a company's Workspace holds the account.
	endpoint = c.UserInfo
	if endpoint == "" {
		endpoint = UserInfoURL
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Identity{}, time.Time{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	var user struct {
		Email string `json:"email"`
		HD    string `json:"hd"`
	}
	if err := c.fetch(req, "userinfo", &user); err != nil {
		return Identity{}, time.Time{}, err
	}
	if !strings.EqualFold(user.Email, info.Email) {
		return Identity{}, time.Time{}, ErrInvalid
	}
	until := time.Unix(exp, 0)
	if until.After(now.Add(MaxTTL)) {
		until = now.Add(MaxTTL)
	}
	return Identity{Email: strings.ToLower(info.Email), HD: strings.ToLower(user.HD)}, until, nil
}

// fetch sends req and decodes its JSON answer into v. Google refusing is
// ErrInvalid; Google failing (5xx, 429, no answer) is another error, never
// remembered as a refusal.
func (c *Client) fetch(req *http.Request, what string, v any) error {
	resp, err := c.http().Do(req)
	if err != nil {
		return fmt.Errorf("googleauth: %s: %w", what, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return fmt.Errorf("googleauth: %s: HTTP %d", what, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return ErrInvalid // an expired, revoked or foreign token
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(v); err != nil {
		return ErrInvalid
	}
	return nil
}

// SignIn is one browser sign-in in progress: what the callback checks.
type SignIn struct {
	State, Verifier, Nonce string
}

func (c *Client) config(redirect string) *oauth2.Config {
	ep := c.Endpoint
	if ep.AuthURL == "" {
		ep = google.Endpoint
	}
	return &oauth2.Config{ClientID: c.ID, ClientSecret: c.Secret, Endpoint: ep, RedirectURL: redirect, Scopes: Scopes}
}

// Start begins a browser sign-in: the URL to send the person to, and what
// the callback must check (keep it in a cookie only this site gets).
func (c *Client) Start(redirect string) (string, SignIn) {
	s := SignIn{State: oauth2.GenerateVerifier(), Verifier: oauth2.GenerateVerifier(), Nonce: oauth2.GenerateVerifier()}
	u := c.config(redirect).AuthCodeURL(s.State, oauth2.S256ChallengeOption(s.Verifier),
		oauth2.SetAuthURLParam("nonce", s.Nonce), oauth2.SetAuthURLParam("prompt", "select_account"))
	return u, s
}

// Finish completes a browser sign-in: the callback's state must be the
// one Start made; the code is exchanged (with the PKCE verifier) and the
// ID token verified.
func (c *Client) Finish(ctx context.Context, redirect string, s SignIn, state, code string) (Identity, error) {
	if s.State == "" || state != s.State || code == "" {
		return Identity{}, ErrInvalid
	}
	ctx = context.WithValue(ctx, oauth2.HTTPClient, c.http())
	tok, err := c.config(redirect).Exchange(ctx, code, oauth2.VerifierOption(s.Verifier))
	if err != nil {
		return Identity{}, fmt.Errorf("%w: the code exchange: %v", ErrInvalid, err)
	}
	raw, _ := tok.Extra("id_token").(string)
	validate := c.ValidateID
	if validate == nil {
		validate = idtoken.Validate
	}
	p, err := validate(ctx, raw, c.ID)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: the ID token: %v", ErrInvalid, err)
	}
	email, _ := p.Claims["email"].(string)
	verified, _ := p.Claims["email_verified"].(bool)
	nonce, _ := p.Claims["nonce"].(string)
	hd, _ := p.Claims["hd"].(string)
	switch {
	case p.Issuer != "accounts.google.com" && p.Issuer != Issuer:
		return Identity{}, fmt.Errorf("%w: issuer %q", ErrInvalid, p.Issuer)
	case nonce != s.Nonce:
		return Identity{}, fmt.Errorf("%w: the nonce", ErrInvalid)
	case email == "" || !verified:
		return Identity{}, fmt.Errorf("%w: no verified email", ErrInvalid)
	}
	return Identity{Email: strings.ToLower(email), HD: strings.ToLower(hd)}, nil
}

// Member reports whether id is one of members: an exact address, or
// "@domain" for every account of that Workspace domain. A domain entry
// needs Google's word that the domain holds the account (hd): a consumer
// Google account can carry any address that once received its
// verification mail (a help desk's, say), so the email alone never does.
func (id Identity) Member(members []string) bool {
	email := strings.ToLower(id.Email)
	at := strings.LastIndexByte(email, '@')
	if at <= 0 {
		return false
	}
	for _, m := range members {
		m = strings.ToLower(strings.TrimSpace(m))
		switch {
		case m == email:
			return true
		case strings.HasPrefix(m, "@") && m == email[at:] && m[1:] == strings.ToLower(id.HD):
			return true
		}
	}
	return false
}
