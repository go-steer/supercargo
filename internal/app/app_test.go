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

package app

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/config"
)

func keyFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "key.pem")
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	if err := config.WriteSigningKey(p, priv); err != nil {
		t.Fatal(err)
	}
	return p
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func cfg(t *testing.T, role string) *config.Config {
	t.Helper()
	y := `domain: cymbal-shoes.example
name: Cymbal Shoes
role: ` + role + `
listen: "` + freePort(t) + `"
tls: { mode: insecure-dev }
keys: [{ kid: cy-1, privateKeyFile: "` + keyFile(t) + `" }]
peers: { ocean-blue.example: "127.0.0.1:1" }
`
	if role == config.RoleOperator {
		y += "operator: { refPrefix: OB, containerPrefix: OCBU, vessel: V, voyage: 1W, departureHour: 18, transit: 1h, deny: [mallory.example] }\n"
	}
	c, err := config.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestNewRoles(t *testing.T) {
	a, err := New(cfg(t, config.RoleShipper), quiet())
	if err != nil {
		t.Fatal(err)
	}
	if a.Shipper == nil || a.Node.Domain() != "cymbal-shoes.example" {
		t.Errorf("shipper app: %+v", a)
	}
	if a.Server.ReadHeaderTimeout == 0 || a.Server.ReadTimeout == 0 || a.Server.WriteTimeout == 0 || a.Server.IdleTimeout == 0 || a.Server.MaxHeaderBytes == 0 {
		t.Error("the HTTP server is missing a timeout or header cap")
	}
	a, err = New(cfg(t, config.RoleOperator), quiet())
	if err != nil {
		t.Fatal(err)
	}
	if a.Shipper != nil {
		t.Error("an operator app has a Shipper")
	}
}

func TestNewRejects(t *testing.T) {
	c := cfg(t, config.RoleShipper)
	c.Keys[0].PrivateKeyFile = filepath.Join(t.TempDir(), "missing.pem")
	if _, err := New(c, quiet()); err == nil {
		t.Error("New with a missing key succeeded")
	}
	c = cfg(t, config.RoleShipper)
	c.TLS.CAFile = filepath.Join(t.TempDir(), "missing.pem")
	if _, err := New(c, quiet()); err == nil {
		t.Error("New with a missing CA file succeeded")
	}
	c = cfg(t, config.RoleShipper)
	junk := filepath.Join(t.TempDir(), "ca.pem")
	_ = os.WriteFile(junk, []byte("not a certificate"), 0o600)
	c.TLS.CAFile = junk
	if _, err := New(c, quiet()); err == nil {
		t.Error("New with a CA file holding no certificates succeeded")
	}
}

func TestServeInsecureStartsAndStops(t *testing.T) {
	var logs bytes.Buffer
	a, err := New(cfg(t, config.RoleOperator), slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Serve(ctx) }()
	waitListening(t, a.Config.Listen)
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Serve: %v", err)
	}
	out := logs.String()
	if !strings.Contains(out, "INSECURE DEVELOPMENT MODE") || !strings.Contains(out, "peer map entry") || !strings.Contains(out, "ocean-blue.example") {
		t.Errorf("startup log lacks the insecure-mode warning or the peer map:\n%s", out)
	}
}

func waitListening(t *testing.T, addr string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			return
		}
	}
	t.Fatalf("nothing listening on %s", addr)
}

// tlsFiles writes a CA and a server certificate for domains.
func tlsFiles(t *testing.T, domains ...string) (certFile, keyFile, caFile string) {
	t.Helper()
	dir := t.TempDir()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	ca, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: domains,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	keyDER, _ := x509.MarshalPKCS8PrivateKey(key)
	certFile, keyFile, caFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"), filepath.Join(dir, "ca.pem")
	_ = os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	_ = os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600)
	_ = os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600)
	return certFile, keyFile, caFile
}

func TestServeTLS(t *testing.T) {
	c := cfg(t, config.RoleOperator)
	c.TLS.Mode = config.TLSLocalCA
	c.TLS.CertFile, c.TLS.KeyFile, c.TLS.CAFile = tlsFiles(t, c.Domain)
	a, err := New(c, quiet())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Serve(ctx) }()
	waitListening(t, c.Listen)
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Serve over TLS: %v", err)
	}
	// A listen address already taken is reported, not swallowed.
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	c2 := cfg(t, config.RoleShipper)
	c2.Listen = l.Addr().String()
	a2, _ := New(c2, quiet())
	if err := a2.Serve(context.Background()); err == nil {
		t.Error("Serve on a taken address returned nil")
	}
}
