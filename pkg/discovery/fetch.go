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

package discovery

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/internal/logsafe"
	"github.com/go-steer/supercargo/pkg/internal/netguard"
	"github.com/go-steer/supercargo/pkg/sig"
)

// FetcherOptions configures a Fetcher.
type FetcherOptions struct {
	// Peers maps a domain to the host:port to connect to for it, instead of
	// resolving it and refusing non-public addresses. It is for local
	// deployments (docker compose, tests) whose nodes live on private
	// addresses, and it changes nothing else: TLS still verifies the domain.
	Peers map[string]string
	// AllowHTTP fetches over plain http and accepts http endpoints. It is
	// for insecure development mode only: without TLS, nothing ties a
	// profile, or its keys, to its domain.
	AllowHTTP bool
	// TLSConfig sets trusted roots (a local CA, say). Nil means the system
	// roots.
	TLSConfig *tls.Config
	// Timeout bounds one fetch. Zero means 10s.
	Timeout time.Duration
	// Algorithms are the key algorithms accepted. Nil means
	// sig.DefaultAlgorithms().
	Algorithms sig.Algorithms
	// Resolver replaces net.DefaultResolver (tests).
	Resolver netguard.Resolver
}

// Fetcher retrieves and validates profiles.
type Fetcher struct {
	client    *http.Client
	allowHTTP bool
	algs      sig.Algorithms
}

// NewFetcher returns a Fetcher. Its HTTP client connects only to public
// addresses (or configured peers), uses no proxy and follows no redirects.
func NewFetcher(opts FetcherOptions) (*Fetcher, error) {
	client, err := netguard.NewClient(netguard.Options{
		Peers:     opts.Peers,
		TLSConfig: opts.TLSConfig,
		Timeout:   opts.Timeout,
		Resolver:  opts.Resolver,
	})
	if err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}
	algs := opts.Algorithms
	if algs == nil {
		algs = sig.DefaultAlgorithms()
	}
	return &Fetcher{client: client, allowHTTP: opts.AllowHTTP, algs: algs}, nil
}

// Fetch retrieves https://{domain}/.well-known/ulp and returns the
// validated profile.
func (f *Fetcher) Fetch(ctx context.Context, domain string) (*Document, error) {
	if !envelope.ValidDomain(domain) {
		return nil, fmt.Errorf("discovery: %s is not a domain", logsafe.Quote(domain))
	}
	scheme := "https"
	if f.allowHTTP {
		scheme = "http"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, scheme+"://"+domain+Path, nil)
	if err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("discovery: fetching the profile of %s: %w", domain, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("discovery: profile of %s: HTTP %d (redirects are not followed)", domain, resp.StatusCode)
	}
	if mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		return nil, fmt.Errorf("discovery: profile of %s: content type %s is not application/json", domain, logsafe.Quote(resp.Header.Get("Content-Type")))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxProfileSize+1))
	if err != nil {
		return nil, fmt.Errorf("discovery: reading the profile of %s: %w", domain, err)
	}
	if len(body) > MaxProfileSize {
		return nil, errors.New("discovery: profile of " + domain + " is over the size limit")
	}
	return ParseProfile(body, ParseOptions{Domain: domain, Algorithms: f.algs, AllowHTTP: f.allowHTTP})
}
