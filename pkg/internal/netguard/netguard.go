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

// Package netguard is the outbound HTTP client for every request a node
// makes to a host named by someone else: fetching a sender's profile,
// posting to a partner's endpoint, delivering a webhook. Those names come
// from unauthenticated messages or partner registrations, so the client
// refuses to reach anything that isn't a public address: loopback, private,
// link-local (cloud metadata included), and the other special-purpose
// ranges.
//
// The check runs on the addresses a name resolves to, and the connection
// goes to the address that was checked, so a name that resolves to a public
// address for the check and a private one for the connection (DNS
// rebinding) gets nowhere. Proxies and redirects are disabled: either
// would move the request past the check.
//
// Peers is the one way around it, and it is explicit configuration: a
// static map from a domain to the host:port to dial for it, for local
// deployments (docker compose, tests) whose nodes live on private
// addresses. Nothing else about the request changes. TLS still verifies the
// domain, and every message is still verified on its own.
package netguard

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"time"

	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/internal/logsafe"
)

// ErrRefused reports a destination the guard will not connect to.
var ErrRefused = errors.New("netguard: destination refused")

// Resolver looks up a host's addresses. *net.Resolver implements it.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Options configures a client.
type Options struct {
	// Peers maps a domain to the host:port dialed for it instead of
	// resolving it, bypassing the address check. Local deployments only.
	Peers map[string]string
	// TLSConfig is cloned for the client (for a private CA, say). TLS 1.2 is
	// the minimum either way.
	TLSConfig *tls.Config
	// Timeout bounds a whole request. Zero means 10s.
	Timeout time.Duration
	// Resolver replaces net.DefaultResolver (tests).
	Resolver Resolver
}

// blocked are special-purpose ranges netip's predicates don't cover.
var blocked = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),   // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // documentation
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation
	netip.MustParsePrefix("192.88.99.0/24"),  // 6to4 relay anycast
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, and broadcast
	netip.MustParsePrefix("2001::/23"),       // IETF protocol assignments, Teredo (embeds IPv4) and benchmarking
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
	netip.MustParsePrefix("2002::/16"),       // 6to4: embeds an IPv4 address
	netip.MustParsePrefix("3fff::/20"),       // documentation (RFC 9637)
}

// globalUnicast6 is the only IPv6 space allowed at all. Everything outside
// it (IPv4-compatible ::/96, SIIT ::ffff:0:0/96, NAT64 64:ff9b::/96,
// discard 100::/64, SRv6 5f00::/16, ULA, link-local, site-local, multicast)
// is refused without needing its own entry.
var globalUnicast6 = netip.MustParsePrefix("2000::/3")

// Allowed reports whether a is a public unicast address.
func Allowed(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || a.IsUnspecified() || a.IsLoopback() || a.IsPrivate() ||
		a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsInterfaceLocalMulticast() || a.IsMulticast() {
		return false
	}
	if a.Is6() && !globalUnicast6.Contains(a) {
		return false
	}
	for _, p := range blocked {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// Dialer connects only to allowed addresses, or to a configured peer.
type Dialer struct {
	peers    map[string]string
	resolver Resolver
	dialer   net.Dialer
}

// NewDialer returns a Dialer for opts. Peer entries are validated: each key
// must be a canonical domain (the form the transport will look up; an IP
// literal would exempt that address from the check), and each target a
// host:port.
func NewDialer(opts Options) (*Dialer, error) {
	d := &Dialer{peers: map[string]string{}, resolver: opts.Resolver, dialer: net.Dialer{Timeout: 5 * time.Second}}
	if d.resolver == nil {
		d.resolver = net.DefaultResolver
	}
	for domain, target := range opts.Peers {
		if !envelope.ValidDomain(domain) {
			return nil, fmt.Errorf("netguard: peer %s is not a lowercase domain", logsafe.Quote(domain))
		}
		if _, _, err := net.SplitHostPort(target); err != nil {
			return nil, fmt.Errorf("netguard: peer %s: target %s is not host:port", logsafe.Quote(domain), logsafe.Quote(target))
		}
		d.peers[domain] = target
	}
	return d, nil
}

// DialContext is a net.Dialer.DialContext for http.Transport.
func (d *Dialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("%w: %s is not host:port", ErrRefused, logsafe.Quote(addr))
	}
	if target, ok := d.peers[host]; ok {
		return d.dialer.DialContext(ctx, network, target)
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		// Names come from validated domains; a literal is checked all the same.
		if !Allowed(ip) {
			return nil, fmt.Errorf("%w: %s is not a public address", ErrRefused, ip)
		}
		return d.dialer.DialContext(ctx, network, addr)
	}
	ips, err := d.resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("netguard: resolving %s: %w", logsafe.Quote(host), err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("netguard: %s has no addresses", logsafe.Quote(host))
	}
	// One non-public address condemns the name: a mix is either a
	// misconfiguration or an attempt to get lucky.
	for _, ip := range ips {
		if !Allowed(ip) {
			return nil, fmt.Errorf("%w: %s resolves to %s, not a public address", ErrRefused, logsafe.Quote(host), ip)
		}
	}
	var errs []error
	for _, ip := range ips {
		conn, err := d.dialer.DialContext(ctx, network, net.JoinHostPort(ip.Unmap().String(), port))
		if err == nil {
			return conn, nil
		}
		errs = append(errs, err)
	}
	return nil, errors.Join(errs...)
}

// NewClient returns an HTTP client that dials through a Dialer, uses no
// proxy, follows no redirect (the 3xx response is returned as is), and
// bounds header size and total time.
func NewClient(opts Options) (*http.Client, error) {
	d, err := NewDialer(opts)
	if err != nil {
		return nil, err
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if opts.TLSConfig != nil {
		// TLS is what ties a host to its domain here, so the config may add
		// trust (RootCAs) but not weaken or redirect verification.
		if opts.TLSConfig.InsecureSkipVerify || opts.TLSConfig.ServerName != "" || opts.TLSConfig.VerifyConnection != nil ||
			opts.TLSConfig.VerifyPeerCertificate != nil {
			return nil, errors.New("netguard: TLSConfig may set trusted roots, not InsecureSkipVerify, ServerName or custom verification")
		}
		tlsConfig = opts.TLSConfig.Clone()
		if tlsConfig.MinVersion < tls.VersionTLS12 {
			tlsConfig.MinVersion = tls.VersionTLS12
		}
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                  nil,
			DialContext:            d.DialContext,
			TLSClientConfig:        tlsConfig,
			ForceAttemptHTTP2:      true,
			MaxResponseHeaderBytes: 16 << 10,
			ResponseHeaderTimeout:  timeout,
			TLSHandshakeTimeout:    5 * time.Second,
			IdleConnTimeout:        90 * time.Second,
		},
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}
