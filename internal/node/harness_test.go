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

package node_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/llm/fake"
	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/internal/operator"
	"github.com/go-steer/supercargo/internal/shipper"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/discovery"
	"github.com/go-steer/supercargo/pkg/sig"
	"github.com/go-steer/supercargo/pkg/transport/rest"
)

const (
	shipperDomain  = "cymbal-shoes.example"
	operatorDomain = "ocean-blue.example"
)

// noDNS resolves nothing: every domain a test reaches is in the peer map,
// and anything else (mallory.example) fails to resolve, offline.
type noDNS struct{}

func (noDNS) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return nil, errors.New("no such host")
}

// testCA mints a CA and server certificates for test domains.
type testCA struct {
	key  *ecdsa.PrivateKey
	cert *x509.Certificate
	pool *x509.CertPool
}

func newCA(t *testing.T) *testCA {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "supercargo test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &testCA{key: key, cert: cert, pool: pool}
}

func (ca *testCA) serverCert(t *testing.T, name string) tls.Certificate {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), DNSNames: []string{name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// company is one party in a test: its keys, its TLS server, and (once
// wired) its node.
type company struct {
	domain string
	kid    string
	priv   ed25519.PrivateKey
	signer sig.Signer
	jwk    json.RawMessage
	srv    *httptest.Server
	addr   string // host:port of srv, for peer maps

	mu      sync.Mutex
	handler http.Handler
	node    *node.Node
	events  []node.Event
}

func newCompany(t *testing.T, ca *testCA, domain, kid string) *company {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	s, _ := sig.NewEd25519Signer(priv)
	j, err := sig.Ed25519JWK(pub, kid)
	if err != nil {
		t.Fatal(err)
	}
	c := &company{domain: domain, kid: kid, priv: priv, signer: s, jwk: j}
	c.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		h := c.handler
		c.mu.Unlock()
		h.ServeHTTP(w, r)
	}))
	c.srv.TLS = &tls.Config{Certificates: []tls.Certificate{ca.serverCert(t, domain)}}
	c.srv.StartTLS()
	t.Cleanup(c.srv.Close)
	c.addr = strings.TrimPrefix(c.srv.URL, "https://")
	return c
}

func (c *company) observe(e node.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
}

// lastIn is the last inbound event the company's node recorded.
func (c *company) lastIn(t *testing.T) node.Event {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.events) - 1; i >= 0; i-- {
		if c.events[i].Direction == "in" {
			return c.events[i]
		}
	}
	t.Fatal("no inbound event recorded")
	return node.Event{}
}

// inCount is how many inbound events the company's node recorded.
func (c *company) inCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, e := range c.events {
		if e.Direction == "in" {
			n++
		}
	}
	return n
}

// network is a set of companies that can reach each other's servers.
type network struct {
	ca    *testCA
	peers map[string]string
	now   func() time.Time
}

func (nw *network) clients(t *testing.T) (*discovery.Cache, *rest.Client) {
	t.Helper()
	tlsConf := &tls.Config{RootCAs: nw.ca.pool}
	f, err := discovery.NewFetcher(discovery.FetcherOptions{Peers: nw.peers, TLSConfig: tlsConf, Resolver: noDNS{}})
	if err != nil {
		t.Fatal(err)
	}
	cache, err := discovery.NewCache(f, discovery.CacheOptions{})
	if err != nil {
		t.Fatal(err)
	}
	client, err := rest.NewClient(cache, rest.ClientOptions{Peers: nw.peers, TLSConfig: tlsConf, Resolver: noDNS{}})
	if err != nil {
		t.Fatal(err)
	}
	return cache, client
}

// wire builds c's node with handlers and serves it.
func (nw *network) wire(t *testing.T, c *company, name, role string, handlers map[string]node.Handler) {
	t.Helper()
	cache, client := nw.clients(t)
	n, err := node.New(node.Config{
		Domain: c.domain, Name: name, Roles: []string{role},
		KID: c.kid, Signer: c.signer, PublicJWK: c.jwk,
		Keys: cache, Replay: store.NewReplay(store.ReplayOptions{}),
		Handlers: handlers, Poster: client, Observer: c.observe, Now: nw.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.node, c.handler = n, n.Handler()
	c.mu.Unlock()
}

// demo is a shipper and an operator wired together, plus Mallory: a party
// with a valid key and server whose domain nobody resolves.
type demo struct {
	nw       *network
	shipper  *company
	operator *company
	mallory  *company
	ship     *shipper.Shipper
	bookings *store.Bookings // the operator's
	booking  *operator.BookingService

	// The push path: the operator's publisher, the shipper's inbound
	// handler and what each logged, and a second carrier (resolvable,
	// verifiable) that has no business pushing to the operator's bookings.
	publisher *operator.Publisher
	opLog     *store.EventLog
	inbound   *shipper.Inbound
	shipLog   *store.EventLog
	rival     *company

	// The inquiry path: the operator's responder and its (fake) model.
	inquiry *operator.InquiryService
	model   *fake.Model

	dropMu  sync.Mutex
	dropped []string
}

func (d *demo) drops() []string {
	d.dropMu.Lock()
	defer d.dropMu.Unlock()
	return append([]string(nil), d.dropped...)
}

func newDemo(t *testing.T) *demo {
	t.Helper()
	ca := newCA(t)
	d := &demo{}
	d.shipper = newCompany(t, ca, shipperDomain, "cy-2026-1")
	d.operator = newCompany(t, ca, operatorDomain, "ob-2026-1")
	d.mallory = newCompany(t, ca, "mallory.example", "ml-1")
	d.rival = newCompany(t, ca, "other-carrier.example", "oc-1")
	d.nw = &network{ca: ca, now: time.Now, peers: map[string]string{
		shipperDomain:           d.shipper.addr,
		operatorDomain:          d.operator.addr,
		"other-carrier.example": d.rival.addr,
	}}
	d.bookings = store.NewBookings()
	d.booking = &operator.BookingService{
		Bookings: d.bookings, RefPrefix: "OB-BKG", ContainerPrefix: "OCBU", Now: time.Now,
		Deny:   map[string]bool{"blocked.example": true},
		Voyage: operator.Voyage{Vessel: "Ocean Blue Aurora", Number: "42W", DepartureHour: 18, Transit: 134 * time.Hour},
	}
	d.opLog = store.NewEventLog(0)
	d.model = operator.FakeInquiryModel()
	d.inquiry = &operator.InquiryService{Bookings: d.bookings, Log: d.opLog, Model: d.model, Name: "Ocean Blue"}
	d.nw.wire(t, d.operator, "Ocean Blue", "operator", map[string]node.Handler{
		"ulp.booking.request": d.booking, "ulp.tnt.inquiry": d.inquiry,
	})
	shipBookings := store.NewBookings()
	d.shipLog = store.NewEventLog(0)
	d.inbound = &shipper.Inbound{Bookings: shipBookings, Log: d.shipLog, Now: time.Now}
	d.nw.wire(t, d.shipper, "Cymbal Shoes", "shipper", map[string]node.Handler{
		"ulp.tnt.event": d.inbound, "ulp.tnt.alert": d.inbound,
	})
	d.nw.wire(t, d.mallory, "Mallory", "shipper", nil)
	d.nw.wire(t, d.rival, "Other Carrier", "operator", nil)
	d.ship = &shipper.Shipper{Node: d.shipper.node, Bookings: shipBookings, Events: d.shipLog, Now: time.Now}
	d.publisher = &operator.Publisher{
		Pusher: d.operator.node, Domain: operatorDomain, Log: d.opLog, Now: time.Now,
		Backoff: []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond},
		OnDrop: func(_, id string, err error) {
			d.dropMu.Lock()
			d.dropped = append(d.dropped, id+": "+err.Error())
			d.dropMu.Unlock()
		},
	}
	t.Cleanup(d.publisher.Close)
	return d
}
