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

package netguard

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestAllowed(t *testing.T) {
	for _, a := range []string{
		"8.8.8.8", "1.1.1.1", "93.184.215.14", "2606:4700:4700::1111", "2a00:1450:4001::200e",
	} {
		if !Allowed(netip.MustParseAddr(a)) {
			t.Errorf("Allowed(%s) = false, want true", a)
		}
	}
	for _, a := range []string{
		"127.0.0.1", "127.255.255.254", "::1", // loopback
		"10.1.2.3", "172.16.0.1", "172.31.255.255", "192.168.1.1", "fd00::1", "fc00::1", // private
		"169.254.169.254", "169.254.0.1", "fe80::1", // link-local, cloud metadata
		"fd00:ec2::254",            // AWS IPv6 metadata (ULA)
		"0.0.0.0", "::", "0.1.2.3", // unspecified, "this network"
		"100.64.0.1", "100.127.255.255", // CGNAT
		"192.0.0.1", "192.0.2.1", "198.18.0.1", "198.51.100.1", "203.0.113.1", // special and documentation
		"240.0.0.1", "255.255.255.255", // reserved, broadcast
		"224.0.0.1", "ff02::1", "ff01::1", // multicast
		"::ffff:127.0.0.1", "::ffff:10.0.0.1", "::ffff:169.254.169.254", // IPv4-mapped
		"64:ff9b::a00:1", "64:ff9b:1::1", "2001::1", "2002:a00:1::", // NAT64, Teredo, 6to4
		"2001:db8::1", "100::1", "fec0::1", // documentation, discard, site-local
	} {
		if Allowed(netip.MustParseAddr(a)) {
			t.Errorf("Allowed(%s) = true, want false", a)
		}
	}
	if Allowed(netip.Addr{}) {
		t.Error("Allowed(invalid) = true")
	}
}

type fakeResolver map[string][]netip.Addr

func (f fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	ips, ok := f[host]
	if !ok {
		return nil, errors.New("no such host")
	}
	return ips, nil
}

func addrs(ss ...string) []netip.Addr {
	var out []netip.Addr
	for _, s := range ss {
		out = append(out, netip.MustParseAddr(s))
	}
	return out
}

func TestDialRefuses(t *testing.T) {
	d, err := NewDialer(Options{Resolver: fakeResolver{
		"internal.example":   addrs("10.0.0.5"),
		"metadata.example":   addrs("169.254.169.254"),
		"mixed.example":      addrs("8.8.8.8", "127.0.0.1"),
		"mapped.example":     addrs("::ffff:192.168.0.1"),
		"empty.example":      nil,
		"rebind-lo.example":  addrs("127.0.0.1"),
		"localhost.example":  addrs("::1"),
		"cgnat.example":      addrs("100.64.1.1"),
		"broadcast.example":  addrs("255.255.255.255"),
		"nat64-lo.example":   addrs("64:ff9b::7f00:1"),
		"documented.example": addrs("2001:db8::5"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, addr := range []string{
		"internal.example:443", "metadata.example:80", "mixed.example:443", "mapped.example:443",
		"rebind-lo.example:443", "localhost.example:443", "cgnat.example:443", "broadcast.example:443",
		"nat64-lo.example:443", "documented.example:443",
		"127.0.0.1:443", "[::1]:443", "169.254.169.254:80", "[::ffff:10.0.0.1]:443",
	} {
		t.Run(addr, func(t *testing.T) {
			conn, err := d.DialContext(context.Background(), "tcp", addr)
			if conn != nil {
				conn.Close()
			}
			if !errors.Is(err, ErrRefused) {
				t.Errorf("DialContext = %v, want ErrRefused", err)
			}
		})
	}
	for _, addr := range []string{"empty.example:443", "unknown.example:443", "no-port.example"} {
		if conn, err := d.DialContext(context.Background(), "tcp", addr); err == nil {
			conn.Close()
			t.Errorf("DialContext(%s) succeeded", addr)
		}
	}
}

// TestDialPublicPassesTheCheck: a public address gets past the guard to a
// real dial, which a cancelled context stops before any packet leaves.
func TestDialPublicPassesTheCheck(t *testing.T) {
	d, _ := NewDialer(Options{Resolver: fakeResolver{"public.example": addrs("8.8.8.8", "2606:4700:4700::1111")}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, addr := range []string{"public.example:443", "8.8.8.8:443"} {
		_, err := d.DialContext(ctx, "tcp", addr)
		if err == nil || errors.Is(err, ErrRefused) {
			t.Errorf("DialContext(%s) = %v; want a dial error, not a refusal", addr, err)
		}
	}
}

func TestPeers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("hello " + r.Host))
	}))
	defer srv.Close()
	target := strings.TrimPrefix(srv.URL, "http://")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1") // must be ignored
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")

	c, err := NewClient(Options{Peers: map[string]string{"ocean-blue.example": target}, Resolver: fakeResolver{}})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Get("http://ocean-blue.example/hello")
	if err != nil {
		t.Fatalf("Get through peer map: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status %d", resp.StatusCode)
	}
	// A redirect comes back as the response; nothing follows it.
	resp, err = c.Get("http://ocean-blue.example/redirect")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("redirect was followed: status %d", resp.StatusCode)
	}
	// A domain not in the map gets no exemption.
	if _, err := c.Get("http://other.example/"); err == nil {
		t.Error("a domain outside the peer map was reached")
	}
	if _, err := NewClient(Options{Peers: map[string]string{"x.example": "no-port"}}); err == nil {
		t.Error("a peer target without a port was accepted")
	}
}

func TestClientDefaults(t *testing.T) {
	pool := x509.NewCertPool()
	c, err := NewClient(Options{TLSConfig: &tls.Config{MinVersion: tls.VersionTLS10, RootCAs: pool}, Timeout: -time.Second})
	if err != nil {
		t.Fatal(err)
	}
	tr := c.Transport.(*http.Transport)
	if tr.Proxy != nil || tr.TLSClientConfig.MinVersion != tls.VersionTLS12 || c.Timeout != 10*time.Second || c.CheckRedirect == nil {
		t.Errorf("client defaults: proxy=%v minTLS=%x timeout=%v", tr.Proxy != nil, tr.TLSClientConfig.MinVersion, c.Timeout)
	}
	if tr.TLSClientConfig.RootCAs != pool {
		t.Error("trusted roots not carried over")
	}
}

// TestClientRefusesWeakTLS: a TLS config may add trust, not switch off or
// redirect verification, which is what ties a host to its domain.
func TestClientRefusesWeakTLS(t *testing.T) {
	for name, cfg := range map[string]*tls.Config{
		"InsecureSkipVerify":    {InsecureSkipVerify: true}, //nolint:gosec // the point of the test
		"ServerName":            {ServerName: "ocean-blue.example"},
		"VerifyConnection":      {VerifyConnection: func(tls.ConnectionState) error { return nil }},
		"VerifyPeerCertificate": {VerifyPeerCertificate: func([][]byte, [][]*x509.Certificate) error { return nil }},
	} {
		if _, err := NewClient(Options{TLSConfig: cfg}); err == nil {
			t.Errorf("NewClient accepted a TLS config with %s", name)
		}
	}
}

func TestPeerKeysMustBeDomains(t *testing.T) {
	for _, key := range []string{"127.0.0.1", "[::1]", "Ocean-Blue.example", "ocean-blue.example.", "localhost", ""} {
		if _, err := NewDialer(Options{Peers: map[string]string{key: "127.0.0.1:8443"}}); err == nil {
			t.Errorf("peer key %q accepted", key)
		}
	}
}

func TestAllowedIPv6Space(t *testing.T) {
	for _, a := range []string{
		"::127.0.0.1", "::a9fe:a9fe", // IPv4-compatible
		"::ffff:0:7f00:1",        // SIIT
		"3fff::1",                // documentation (RFC 9637)
		"5f00::1",                // SRv6
		"2001:2::1", "2001:1::1", // IETF assignments, benchmarking
		"4000::1", "1000::1", // outside global unicast
		"::ffff:100.64.0.1", "::ffff:192.88.99.1", // IPv4-mapped forms of blocked ranges
		"192.88.99.1",
	} {
		if Allowed(netip.MustParseAddr(a)) {
			t.Errorf("Allowed(%s) = true, want false", a)
		}
	}
}
