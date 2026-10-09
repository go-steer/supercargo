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
//     tokeninfo endpoint (issued to our client, not expired, a verified
//     email), and the result cached until the token expires.
//   - The browser UI's sign-in: the authorization-code flow with PKCE and a
//     nonce, server-side, whose ID token is verified (Google's signature,
//     issuer, our client as the audience, the nonce, a verified email).
//
// Either way the outcome is an email, which the caller maps to the roles
// it allows; this package decides nothing about access.
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
)

// ErrInvalid is a token or a sign-in that doesn't hold.
var ErrInvalid = errors.New("googleauth: invalid")

// Identity is who a token or a sign-in belongs to.
type Identity struct {
	Email string
}

// Client is the company's OAuth client.
type Client struct {
	ID, Secret string
	// HTTP fetches tokeninfo and exchanges codes (default: 10 s timeout).
	HTTP *http.Client
	// TokenInfoURL and the endpoint replace Google's (tests).
	TokenInfo string
	Endpoint  oauth2.Endpoint
	// ValidateID replaces Google's ID token check (tests).
	ValidateID func(ctx context.Context, token, audience string) (*idtoken.Payload, error)
	Now        func() time.Time

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	id      Identity
	expires time.Time
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
// to this client, unexpired, for a verified email.
func (c *Client) AccessToken(ctx context.Context, token string) (Identity, error) {
	if token == "" || len(token) > 4096 {
		return Identity{}, ErrInvalid
	}
	sum := sha256.Sum256([]byte(token))
	key := hex.EncodeToString(sum[:])
	now := c.now()
	c.mu.Lock()
	if e, ok := c.cache[key]; ok && now.Before(e.expires) {
		c.mu.Unlock()
		return e.id, nil
	}
	c.mu.Unlock()

	endpoint := c.TokenInfo
	if endpoint == "" {
		endpoint = TokenInfoURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(url.Values{"access_token": {token}}.Encode()))
	if err != nil {
		return Identity{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http().Do(req)
	if err != nil {
		return Identity{}, fmt.Errorf("googleauth: tokeninfo: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Identity{}, ErrInvalid // Google says no (an expired or foreign token)
	}
	var info struct {
		Aud           string `json:"aud"`
		Azp           string `json:"azp"`
		Email         string `json:"email"`
		EmailVerified string `json:"email_verified"`
		Exp           string `json:"exp"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&info); err != nil {
		return Identity{}, ErrInvalid
	}
	exp, err := strconv.ParseInt(info.Exp, 10, 64)
	switch {
	case err != nil, !time.Unix(exp, 0).After(now):
		return Identity{}, ErrInvalid
	case info.Aud != c.ID && info.Azp != c.ID:
		// Issued to someone else's client: never accepted here.
		return Identity{}, ErrInvalid
	case info.Email == "" || info.EmailVerified != "true":
		return Identity{}, ErrInvalid
	}
	id := Identity{Email: strings.ToLower(info.Email)}
	until := time.Unix(exp, 0)
	if until.After(now.Add(MaxTTL)) {
		until = now.Add(MaxTTL)
	}
	c.mu.Lock()
	if c.cache == nil {
		c.cache = map[string]cached{}
	}
	if len(c.cache) >= MaxCached {
		for k, e := range c.cache {
			if !now.Before(e.expires) || len(c.cache) >= MaxCached {
				delete(c.cache, k)
			}
		}
	}
	c.cache[key] = cached{id, until}
	c.mu.Unlock()
	return id, nil
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
	switch {
	case p.Issuer != "accounts.google.com" && p.Issuer != Issuer:
		return Identity{}, fmt.Errorf("%w: issuer %q", ErrInvalid, p.Issuer)
	case nonce != s.Nonce:
		return Identity{}, fmt.Errorf("%w: the nonce", ErrInvalid)
	case email == "" || !verified:
		return Identity{}, fmt.Errorf("%w: no verified email", ErrInvalid)
	}
	return Identity{Email: strings.ToLower(email)}, nil
}

// Member reports whether email is one of members: an exact address, or
// "@domain" for everyone in that domain.
func Member(email string, members []string) bool {
	email = strings.ToLower(email)
	at := strings.LastIndexByte(email, '@')
	if at <= 0 {
		return false
	}
	for _, m := range members {
		m = strings.ToLower(strings.TrimSpace(m))
		if m == email || strings.HasPrefix(m, "@") && m == email[at:] {
			return true
		}
	}
	return false
}
