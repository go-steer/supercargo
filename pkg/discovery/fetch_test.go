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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/supercargo/pkg/internal/netguard"
)

// serve returns a server whose handler writes body for Path, and the
// host:port to put in a peer map.
func serve(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func profileHandler(body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != Path {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write(body)
	}
}

func insecureFetcher(t *testing.T, target string) *Fetcher {
	t.Helper()
	f, err := NewFetcher(FetcherOptions{AllowHTTP: true, Peers: map[string]string{domain: target}})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestFetch(t *testing.T) {
	target := serve(t, profileHandler(encode(t, profileMap(t, "ob-2026-1"))))
	d, err := insecureFetcher(t, target).Fetch(context.Background(), domain)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if _, ok := d.Key("ob-2026-1"); !ok {
		t.Error("fetched profile lacks its key")
	}
}

func TestFetchRejects(t *testing.T) {
	good := encode(t, profileMap(t))
	other := profileMap(t)
	other["party"].(map[string]any)["domain"] = "mallory.example"
	for _, tc := range []struct {
		name string
		h    http.HandlerFunc
	}{
		{"not found", http.NotFound},
		// Both carry a valid profile with the right content type, so only the
		// status check can refuse them.
		{"redirect with a valid body", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "http://169.254.169.254/latest/meta-data/")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusFound)
			_, _ = w.Write(good)
		}},
		{"non-200 with a valid body", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write(good)
		}},
		{"server error", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }},
		{"wrong content type", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write(good)
		}},
		{"no content type", func(w http.ResponseWriter, _ *http.Request) {
			w.Header()["Content-Type"] = nil
			_, _ = w.Write(good)
		}},
		{"oversize", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"pad":"` + strings.Repeat("x", MaxProfileSize) + `"}`))
		}},
		{"another party's profile", profileHandler(encode(t, other))},
		{"invalid profile", profileHandler([]byte(`{"protocolVersion":"0.1"}`))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if d, err := insecureFetcher(t, serve(t, tc.h)).Fetch(context.Background(), domain); err == nil {
				t.Errorf("Fetch succeeded: %+v", d.Profile)
			}
		})
	}
	f := insecureFetcher(t, "127.0.0.1:1")
	for _, bad := range []string{"", "localhost", "10.0.0.1", "Ocean-Blue.example", "ocean-blue.example:8443"} {
		if _, err := f.Fetch(context.Background(), bad); err == nil {
			t.Errorf("Fetch(%q) succeeded", bad)
		}
	}
}

type resolver map[string][]netip.Addr

func (r resolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if a, ok := r[host]; ok {
		return a, nil
	}
	return nil, errors.New("no such host")
}

// TestFetchRefusesPrivateAddresses: without a peer entry, a domain that
// resolves inward is refused before any connection.
func TestFetchRefusesPrivateAddresses(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.0.0.8", "169.254.169.254", "::1", "fd00::5"} {
		f, err := NewFetcher(FetcherOptions{Resolver: resolver{domain: {netip.MustParseAddr(ip)}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Fetch(context.Background(), domain); !errors.Is(err, netguard.ErrRefused) {
			t.Errorf("%s resolving to %s: Fetch = %v, want netguard.ErrRefused", domain, ip, err)
		}
	}
	if _, err := NewFetcher(FetcherOptions{Peers: map[string]string{domain: "no-port"}}); err == nil {
		t.Error("NewFetcher accepted a malformed peer target")
	}
}

// tlsServer serves h over TLS with a certificate for names, signed by a CA
// minted here; it returns the CA pool and the host:port.
func tlsServer(t *testing.T, h http.Handler, names ...string) (*x509.CertPool, string) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), DNSNames: names,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return pool, strings.TrimPrefix(srv.URL, "https://")
}

// TestFetchTLS: the peer map changes where to connect, not what TLS
// verifies. A certificate for the domain succeeds; one for another name
// fails.
func TestFetchTLS(t *testing.T) {
	h := profileHandler(encode(t, profileMap(t)))
	pool, target := tlsServer(t, h, domain)
	f, err := NewFetcher(FetcherOptions{Peers: map[string]string{domain: target}, TLSConfig: &tls.Config{RootCAs: pool}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Fetch(context.Background(), domain); err != nil {
		t.Fatalf("Fetch over TLS: %v", err)
	}

	pool, target = tlsServer(t, h, "mallory.example")
	f, _ = NewFetcher(FetcherOptions{Peers: map[string]string{domain: target}, TLSConfig: &tls.Config{RootCAs: pool}})
	if _, err := f.Fetch(context.Background(), domain); err == nil {
		t.Error("Fetch accepted a certificate for another domain")
	}
	// And with the default (system) roots, the test CA isn't trusted.
	f, _ = NewFetcher(FetcherOptions{Peers: map[string]string{domain: target}})
	if _, err := f.Fetch(context.Background(), domain); err == nil {
		t.Error("Fetch trusted a private CA it wasn't given")
	}
}
