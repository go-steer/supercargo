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
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/discovery"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
	"github.com/go-steer/supercargo/pkg/sig"
	"github.com/go-steer/supercargo/pkg/transport/rest"
)

// staticKeys resolves exactly the keys it was given.
type staticKeys map[string]sig.Verifier

func (s staticKeys) Key(_ context.Context, domain, kid string) (sig.Verifier, error) {
	if v, ok := s[domain+"/"+kid]; ok {
		return v, nil
	}
	return nil, discovery.ErrUnknownKey
}

type party struct {
	signer sig.Signer
	jwk    json.RawMessage
	v      sig.Verifier
}

func newParty(t *testing.T, kid string) party {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	s, _ := sig.NewEd25519Signer(priv)
	j, _ := sig.Ed25519JWK(pub, kid)
	v, _ := sig.NewEd25519Verifier(pub)
	return party{s, j, v}
}

// handlerFunc adapts functions to node.Handler.
type handlerFunc struct {
	authorize func(context.Context, *node.Request) error
	handle    func(context.Context, *node.Request) (*node.Reply, error)
}

func (h handlerFunc) Authorize(ctx context.Context, r *node.Request) error {
	if h.authorize == nil {
		return nil
	}
	return h.authorize(ctx, r)
}

func (h handlerFunc) Handle(ctx context.Context, r *node.Request) (*node.Reply, error) {
	return h.handle(ctx, r)
}

func baseConfig(t *testing.T, op party, keys staticKeys, h node.Handler) node.Config {
	t.Helper()
	return node.Config{
		Domain: operatorDomain, Name: "Ocean Blue", Roles: []string{"operator"},
		KID: "ob-2026-1", Signer: op.signer, PublicJWK: op.jwk,
		Keys: keys, Replay: store.NewReplay(store.ReplayOptions{}),
		Handlers: map[string]node.Handler{intents.BookingRequest: h},
	}
}

func TestNewValidates(t *testing.T) {
	op := newParty(t, "ob-2026-1")
	other := newParty(t, "ob-2026-1")
	h := handlerFunc{handle: func(context.Context, *node.Request) (*node.Reply, error) { return nil, nil }}
	if _, err := node.New(baseConfig(t, op, staticKeys{}, h)); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	for name, edit := range map[string]func(c *node.Config){
		"bad domain":                 func(c *node.Config) { c.Domain = "Ocean Blue" },
		"no signer":                  func(c *node.Config) { c.Signer = nil },
		"no keys":                    func(c *node.Config) { c.Keys = nil },
		"no replay store":            func(c *node.Config) { c.Replay = nil },
		"bad kid":                    func(c *node.Config) { c.KID = "ob 1" },
		"retention not over 2× skew": func(c *node.Config) { c.Replay = store.NewReplay(store.ReplayOptions{Retention: 10 * time.Minute}) },
		"handler for an undecodable intent": func(c *node.Config) {
			c.Handlers = map[string]node.Handler{"ulp.delegation.revoke": h}
		},
		"nil handler":            func(c *node.Config) { c.Handlers = map[string]node.Handler{intents.BookingRequest: nil} },
		"JWK of another key":     func(c *node.Config) { c.PublicJWK = other.jwk },
		"JWK under another kid":  func(c *node.Config) { c.KID = "ob-2026-2" },
		"REST path not /ulp":     func(c *node.Config) { c.RESTURL = "https://ocean-blue.example/api" },
		"REST on another domain": func(c *node.Config) { c.RESTURL = "https://mallory.example/ulp" },
		"REST over http":         func(c *node.Config) { c.RESTURL = "http://ocean-blue.example/ulp" },
		"no name":                func(c *node.Config) { c.Name = "" },
		"unknown role":           func(c *node.Config) { c.Roles = []string{"admin"} },
	} {
		t.Run(name, func(t *testing.T) {
			c := baseConfig(t, op, staticKeys{}, h)
			edit(&c)
			if _, err := node.New(c); err == nil {
				t.Error("New succeeded")
			}
		})
	}
	c := baseConfig(t, op, staticKeys{}, h)
	c.RESTURL, c.AllowHTTP = "http://ocean-blue.example/ulp", true
	if _, err := node.New(c); err != nil {
		t.Errorf("http endpoint in insecure mode: %v", err)
	}
}

// process sends one booking request from the shipper party straight into
// the node's pipeline and returns the decoded ulp.error, if that's what came
// back.
func process(t *testing.T, n *node.Node, cy party, op party) (*intents.ErrorBody, []byte) {
	t.Helper()
	h := envelope.NewHeader(intents.BookingRequest, shipperDomain, operatorDomain, time.Now())
	b, _ := json.Marshal(bookingRequest())
	msg, err := envelope.Seal(&envelope.Envelope{Header: h, Body: b}, "cy-2026-1", cy.signer)
	if err != nil {
		t.Fatal(err)
	}
	res := n.Process(context.Background(), msg)
	if res.Response == nil {
		return nil, nil
	}
	u, err := envelope.Parse(res.Response)
	if err != nil {
		t.Fatal(err)
	}
	env, err := u.Verify(op.v)
	if err != nil {
		t.Fatal(err)
	}
	body, err := intents.Decode(env)
	if err != nil {
		t.Fatal(err)
	}
	e, _ := body.(*intents.ErrorBody)
	return e, res.Response
}

func TestHandlerFailures(t *testing.T) {
	op, cy := newParty(t, "ob-2026-1"), newParty(t, "cy-2026-1")
	keys := staticKeys{shipperDomain + "/cy-2026-1": cy.v}
	secret := "pq: password authentication failed for user ulp at 10.0.0.12"
	for _, tc := range []struct {
		name      string
		h         handlerFunc
		code      string
		retryable bool
	}{
		{"panics", handlerFunc{handle: func(context.Context, *node.Request) (*node.Reply, error) { panic(secret) }},
			intents.CodeInternal, false}, // Handle may have changed state: not retryable
		{"returns a raw error", handlerFunc{handle: func(context.Context, *node.Request) (*node.Reply, error) {
			return nil, errors.New(secret)
		}}, intents.CodeInternal, false},
		{"authorize panics", handlerFunc{authorize: func(context.Context, *node.Request) error { panic(secret) }},
			intents.CodeInternal, true}, // nothing changed yet: retryable
		{"authorize returns a raw error", handlerFunc{authorize: func(context.Context, *node.Request) error {
			return errors.New(secret)
		}}, intents.CodeForbidden, false},
		{"rejection with an unsafe detail", handlerFunc{authorize: func(context.Context, *node.Request) error {
			return node.Reject(intents.CodeForbidden, "no\nway", secret)
		}}, intents.CodeForbidden, false},
		{"replies with an invalid body", handlerFunc{handle: func(context.Context, *node.Request) (*node.Reply, error) {
			return &node.Reply{Body: &intents.BookingConfirmationBody{BookingRef: "x y", Status: "?"}}, nil
		}}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logged []node.Event
			c := baseConfig(t, op, keys, tc.h)
			c.Observer = func(e node.Event) { logged = append(logged, e) }
			n, err := node.New(c)
			if err != nil {
				t.Fatal(err)
			}
			e, raw := process(t, n, cy, op)
			if tc.code == "" {
				if raw != nil {
					t.Errorf("an invalid reply was sent: %s", raw)
				}
				return
			}
			if e == nil || e.Code != tc.code || e.Retryable != tc.retryable {
				t.Fatalf("reply %+v; want %s, retryable=%v", e, tc.code, tc.retryable)
			}
			// The internal detail stays in the log, never in the signed reply.
			if strings.Contains(e.Detail, "password") || strings.Contains(string(raw), "10.0.0.12") {
				t.Errorf("internal error text leaked into the reply: %q", e.Detail)
			}
			found := false
			for _, ev := range logged {
				found = found || strings.Contains(ev.Reason, "password")
			}
			if !found {
				t.Error("the internal reason didn't reach the log")
			}
		})
	}
}

func TestPushWithNoReply(t *testing.T) {
	op, cy := newParty(t, "ob-2026-1"), newParty(t, "cy-2026-1")
	keys := staticKeys{shipperDomain + "/cy-2026-1": cy.v}
	n, err := node.New(baseConfig(t, op, keys, handlerFunc{handle: func(context.Context, *node.Request) (*node.Reply, error) {
		return nil, nil
	}}))
	if err != nil {
		t.Fatal(err)
	}
	if e, raw := process(t, n, cy, op); e != nil || raw != nil {
		t.Errorf("a handler with no reply produced %s", raw)
	}
}

func TestExchangeMisuse(t *testing.T) {
	op := newParty(t, "ob-2026-1")
	h := handlerFunc{handle: func(context.Context, *node.Request) (*node.Reply, error) { return nil, nil }}
	n, err := node.New(baseConfig(t, op, staticKeys{}, h))
	if err != nil {
		t.Fatal(err)
	}
	hdr := envelope.NewHeader(intents.BookingRequest, operatorDomain, shipperDomain, time.Now())
	if _, err := n.Exchange(context.Background(), hdr, bookingRequest()); err == nil {
		t.Error("Exchange without a Poster succeeded")
	}
	c := baseConfig(t, op, staticKeys{}, h)
	c.Poster = posterFunc(func(context.Context, string, []byte) ([]byte, error) { return nil, errors.New("unreachable") })
	n, _ = node.New(c)
	if _, err := n.Exchange(context.Background(), envelope.NewHeader(intents.BookingRequest, "mallory.example", shipperDomain, time.Now()), bookingRequest()); err == nil {
		t.Error("Exchange as another sender succeeded")
	}
	if _, err := n.Exchange(context.Background(), envelope.NewHeader(intents.Error, operatorDomain, shipperDomain, time.Now()), bookingRequest()); err == nil {
		t.Error("Exchange with a header intent unlike the body's succeeded")
	}
	bad := bookingRequest()
	bad.ShipperRef = ""
	if _, err := n.Exchange(context.Background(), hdr, bad); err == nil {
		t.Error("Exchange with an invalid body succeeded")
	}
	if _, err := n.Exchange(context.Background(), hdr, bookingRequest()); err == nil {
		t.Error("Exchange through a failing Poster succeeded")
	}
}

// TestOutboundGuard: a guard that refuses a body stops the send before
// anything is sealed or posted.
func TestOutboundGuard(t *testing.T) {
	op := newParty(t, "ob-2026-1")
	h := handlerFunc{handle: func(context.Context, *node.Request) (*node.Reply, error) { return nil, nil }}
	hdr := envelope.NewHeader(intents.BookingRequest, operatorDomain, shipperDomain, time.Now())
	posted := 0
	c := baseConfig(t, op, staticKeys{}, h)
	c.Poster = posterFunc(func(context.Context, string, []byte) ([]byte, error) { posted++; return nil, rest.ErrNoResponse })
	c.OutboundGuard = func(_ string, body []byte) error {
		if strings.Contains(string(body), "sneakers") {
			return errors.New("private")
		}
		return nil
	}
	n, _ := node.New(c)
	if _, err := n.Exchange(context.Background(), hdr, bookingRequest()); err == nil || !strings.Contains(err.Error(), "refused to send") {
		t.Errorf("a guarded body: %v, want refused", err)
	}
	if posted != 0 {
		t.Error("a refused body was posted")
	}
	body := bookingRequest()
	body.Cargo.Description = "Footwear"
	if _, err := n.Exchange(context.Background(), hdr, body); err != nil || posted != 1 {
		t.Errorf("an allowed body: %v, posted %d", err, posted)
	}
}

// TestOutboundGuardOnReplies: a reply the guard refuses isn't sent: the
// sender gets a bare 500, nothing signed.
func TestOutboundGuardOnReplies(t *testing.T) {
	op, cy := newParty(t, "ob-2026-1"), newParty(t, "cy-2026-1")
	h := handlerFunc{handle: func(context.Context, *node.Request) (*node.Reply, error) {
		return &node.Reply{Body: confirmationFor("OB-BKG-7"), BookingRef: "OB-BKG-7"}, nil
	}}
	c := baseConfig(t, op, staticKeys{shipperDomain + "/cy-2026-1": cy.v}, h)
	c.OutboundGuard = func(_ string, body []byte) error {
		if strings.Contains(string(body), "OB-BKG-7") {
			return errors.New("private")
		}
		return nil
	}
	var events []node.Event
	c.Observer = func(e node.Event) { events = append(events, e) }
	n, _ := node.New(c)
	hdr := envelope.NewHeader(intents.BookingRequest, shipperDomain, operatorDomain, time.Now())
	b, _ := json.Marshal(bookingRequest())
	msg, _ := envelope.Seal(&envelope.Envelope{Header: hdr, Body: b}, "cy-2026-1", cy.signer)
	if res := n.Process(context.Background(), msg); res.Response != nil || res.Status != http.StatusInternalServerError {
		t.Errorf("a guarded reply: %d with %d bytes, want a bare 500", res.Status, len(res.Response))
	}
	// Accepted, then failed: two events, but the message's bytes travel
	// with the first only, so the message log holds it once.
	var withWire, failed int
	for _, e := range events {
		if e.Wire != nil {
			withWire++
		}
		if e.Outcome == "failed" {
			failed++
		}
	}
	if withWire != 1 || failed != 1 {
		t.Errorf("%d events carry the message, %d failed; want 1, 1", withWire, failed)
	}
}

func TestExchangeEdges(t *testing.T) {
	op, cy := newParty(t, "ob-2026-1"), newParty(t, "cy-2026-1")
	h := handlerFunc{handle: func(context.Context, *node.Request) (*node.Reply, error) { return nil, nil }}
	hdr := envelope.NewHeader(intents.BookingRequest, operatorDomain, shipperDomain, time.Now())

	// No answer expected, none given (a push): fine.
	c := baseConfig(t, op, staticKeys{}, h)
	c.Poster = posterFunc(func(context.Context, string, []byte) ([]byte, error) { return nil, rest.ErrNoResponse })
	n, _ := node.New(c)
	if r, err := n.Exchange(context.Background(), hdr, bookingRequest()); r != nil || err != nil {
		t.Errorf("push answered 204: %v, %v", r, err)
	}
	// An answer expected, none given.
	if _, err := n.Exchange(context.Background(), hdr, bookingRequest(), intents.BookingConfirmation); err == nil {
		t.Error("204 accepted where a confirmation was expected")
	}

	// A well-formed answer whose signer's key can't be resolved.
	c = baseConfig(t, op, staticKeys{}, h) // knows no keys at all
	c.Poster = posterFunc(func(_ context.Context, _ string, msg []byte) ([]byte, error) {
		u, _ := envelope.Parse(msg)
		rh := u.ClaimedHeader().Reply(intents.BookingConfirmation, time.Now())
		ref := "OB-BKG-1"
		rh.BookingRef = &ref
		b, _ := json.Marshal(confirmationFor(ref))
		return envelope.Seal(&envelope.Envelope{Header: rh, Body: b}, "cy-2026-1", cy.signer)
	})
	n, _ = node.New(c)
	if _, err := n.Exchange(context.Background(), hdr, bookingRequest(), intents.BookingConfirmation); !errors.Is(err, node.ErrBadResponse) {
		t.Errorf("response with an unresolvable key: %v, want ErrBadResponse", err)
	}
}

// TestExchangeResponseChecks covers response checks that need a response
// whose key does resolve, so the check under test is the only thing that
// can refuse it.
func TestExchangeResponseChecks(t *testing.T) {
	op, cy, cf := newParty(t, "ob-2026-1"), newParty(t, "cy-2026-1"), newParty(t, "cf-2026-1")
	keys := staticKeys{shipperDomain + "/cy-2026-1": cy.v, "cymbal-footwear.example/cf-2026-1": cf.v}
	h := handlerFunc{handle: func(context.Context, *node.Request) (*node.Reply, error) { return nil, nil }}
	answer := func(sender string, signer sig.Signer, kid, intent string, body any) node.Poster {
		return posterFunc(func(_ context.Context, _ string, msg []byte) ([]byte, error) {
			u, _ := envelope.Parse(msg)
			rh := u.ClaimedHeader().Reply(intent, time.Now())
			rh.Sender = sender
			ref := "OB-BKG-1"
			rh.BookingRef = &ref
			intents.SetDialect(&rh)
			b, _ := json.Marshal(body)
			return envelope.Seal(&envelope.Envelope{Header: rh, Body: b}, kid, signer)
		})
	}
	hdr := envelope.NewHeader(intents.BookingRequest, operatorDomain, shipperDomain, time.Now())
	for name, tc := range map[string]struct {
		poster node.Poster
		expect string
	}{
		// A third party whose key resolves, answering a message sent to the
		// shipper: only the sender check stands in its way.
		"from a third party with a resolvable key": {
			answer("cymbal-footwear.example", cf.signer, "cf-2026-1", intents.BookingConfirmation, confirmationFor("OB-BKG-1")),
			intents.BookingConfirmation},
		// A genuine, decodable confirmation, where an inquiry response was
		// expected: only the expected-intent check refuses it.
		"an intent not expected": {
			answer(shipperDomain, cy.signer, "cy-2026-1", intents.BookingConfirmation, confirmationFor("OB-BKG-1")),
			intents.TnTInquiryResponse},
	} {
		t.Run(name, func(t *testing.T) {
			c := baseConfig(t, op, keys, h)
			c.Poster = tc.poster
			n, _ := node.New(c)
			if _, err := n.Exchange(context.Background(), hdr, bookingRequest(), tc.expect); !errors.Is(err, node.ErrBadResponse) {
				t.Errorf("Exchange = %v, want ErrBadResponse", err)
			}
		})
	}
	// The baseline: the same answer from the right party, expected, passes.
	c := baseConfig(t, op, keys, h)
	c.Poster = answer(shipperDomain, cy.signer, "cy-2026-1", intents.BookingConfirmation, confirmationFor("OB-BKG-1"))
	n, _ := node.New(c)
	if _, err := n.Exchange(context.Background(), hdr, bookingRequest(), intents.BookingConfirmation); err != nil {
		t.Errorf("genuine response: %v", err)
	}
}

// TestExchangeChecksBeforeSending: a bad header or body is refused before
// anything is posted.
func TestExchangeChecksBeforeSending(t *testing.T) {
	op := newParty(t, "ob-2026-1")
	posted := false
	c := baseConfig(t, op, staticKeys{}, handlerFunc{handle: func(context.Context, *node.Request) (*node.Reply, error) { return nil, nil }})
	c.Poster = posterFunc(func(context.Context, string, []byte) ([]byte, error) { posted = true; return nil, rest.ErrNoResponse })
	n, _ := node.New(c)
	bad := bookingRequest()
	bad.ShipperRef = ""
	for name, call := range map[string]func() error{
		"as another sender": func() error {
			_, err := n.Exchange(context.Background(), envelope.NewHeader(intents.BookingRequest, "mallory.example", shipperDomain, time.Now()), bookingRequest())
			return err
		},
		"invalid body": func() error {
			_, err := n.Exchange(context.Background(), envelope.NewHeader(intents.BookingRequest, operatorDomain, shipperDomain, time.Now()), bad)
			return err
		},
	} {
		posted = false
		if err := call(); err == nil || posted {
			t.Errorf("%s: err %v, posted %v; want refused before posting", name, err, posted)
		}
	}
}

// TestReplayStoreFullFailsClosed: when the replay store can't record a
// message, the message is refused (retryably), not admitted unrecorded.
func TestReplayStoreFullFailsClosed(t *testing.T) {
	op, cy := newParty(t, "ob-2026-1"), newParty(t, "cy-2026-1")
	keys := staticKeys{shipperDomain + "/cy-2026-1": cy.v}
	handled := 0
	c := baseConfig(t, op, keys, handlerFunc{handle: func(context.Context, *node.Request) (*node.Reply, error) {
		handled++
		return nil, nil
	}})
	c.Replay = store.NewReplay(store.ReplayOptions{Retention: time.Hour, Max: 1})
	n, err := node.New(c)
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := process(t, n, cy, op); e != nil {
		t.Fatalf("first message: %+v", e)
	}
	e, _ := process(t, n, cy, op)
	if e == nil || e.Code != intents.CodeInternal || !e.Retryable {
		t.Errorf("second message with the store full: %+v, want a retryable internal error", e)
	}
	if handled != 1 {
		t.Errorf("handler ran %d times, want 1", handled)
	}
}

// TestSinceFence: a message sent before the node started is refused as
// stale, retryable, before it is recorded or handled; one sent after is
// taken.
func TestSinceFence(t *testing.T) {
	op, cy := newParty(t, "ob-2026-1"), newParty(t, "cy-2026-1")
	keys := staticKeys{shipperDomain + "/cy-2026-1": cy.v}
	handled := 0
	c := baseConfig(t, op, keys, handlerFunc{handle: func(context.Context, *node.Request) (*node.Reply, error) {
		handled++
		return nil, nil
	}})
	c.Since = time.Now().Add(time.Minute)
	n, err := node.New(c)
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := process(t, n, cy, op); e == nil || e.Code != intents.CodeStale || !e.Retryable || handled != 0 || c.Replay.Len() != 0 {
		t.Errorf("sent before the start: %+v, handled %d, recorded %d", e, handled, c.Replay.Len())
	}
	c.Since = time.Now().Add(-time.Minute)
	if n, err = node.New(c); err != nil {
		t.Fatal(err)
	}
	if e, _ := process(t, n, cy, op); e != nil || handled != 1 {
		t.Errorf("sent after the start: %+v, handled %d", e, handled)
	}
}

type posterFunc func(context.Context, string, []byte) ([]byte, error)

func (f posterFunc) Post(ctx context.Context, r string, m []byte) ([]byte, error) {
	return f(ctx, r, m)
}

func (f posterFunc) PostTo(ctx context.Context, r, _ string, m []byte) ([]byte, error) {
	return f(ctx, r, m)
}

func TestProfileServed(t *testing.T) {
	op := newParty(t, "ob-2026-1")
	h := handlerFunc{handle: func(context.Context, *node.Request) (*node.Reply, error) { return nil, nil }}
	n, err := node.New(baseConfig(t, op, staticKeys{}, h))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(n.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + discovery.Path)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	doc, err := discovery.ParseProfile(b, discovery.ParseOptions{Domain: operatorDomain})
	if err != nil {
		t.Fatalf("served profile: %v", err)
	}
	if !doc.Accepts(intents.BookingRequest) || doc.Accepts(intents.TnTInquiry) {
		t.Error("profile doesn't list exactly the handled intents")
	}
	if _, ok := doc.Key("ob-2026-1"); !ok {
		t.Error("profile doesn't publish the node's key")
	}
	if resp, _ := http.Post(srv.URL+discovery.Path, "application/json", bytes.NewReader(nil)); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST to the profile: HTTP %d", resp.StatusCode)
	}
	// The capabilities follow the handled intents; the dialects are the
	// ones this node decodes.
	c, ok := doc.Capability("dev.ulp.logistics.shipment")
	if !ok || !slices.Equal(c.Tags, []string{"ulp:service", "ulp:dcsa:container-booking", "ulp:transport:rest"}) || c.SpecURL != "" {
		t.Errorf("shipment capability %+v, %v", c, ok)
	}
	if _, ok := doc.Capability("dev.ulp.logistics.tracking"); ok || len(doc.Profile.Capabilities) != 1 {
		t.Errorf("capabilities %+v", doc.Profile.Capabilities)
	}
	if !doc.SupportsDialect("DCSA_TNT") || !doc.SupportsDialect("DCSA_OCEAN_CONTAINER") || doc.SupportsDialect("ULP_ROAD") {
		t.Errorf("supported_dialects %v", doc.Profile.SupportedDialects)
	}
}

// TestProfileCapabilities: a tracking operator serving A2A tags the
// inquiry's transport, and a subscriber lists what it receives.
func TestProfileCapabilities(t *testing.T) {
	op := newParty(t, "ob-2026-1")
	h := handlerFunc{handle: func(context.Context, *node.Request) (*node.Reply, error) { return nil, nil }}
	profile := func(handled []string, a2a string) *discovery.Document {
		t.Helper()
		c := baseConfig(t, op, staticKeys{}, h)
		c.Handlers = map[string]node.Handler{}
		for _, in := range handled {
			c.Handlers[in] = h
		}
		c.A2AURL = a2a
		n, err := node.New(c)
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		n.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, discovery.Path, nil))
		doc, err := discovery.ParseProfile(rec.Body.Bytes(), discovery.ParseOptions{Domain: operatorDomain})
		if err != nil {
			t.Fatal(err)
		}
		return doc
	}
	doc := profile([]string{intents.TnTInquiry, intents.OceanTnTRequest}, "https://"+operatorDomain+"/a2a")
	c, ok := doc.Capability("dev.ulp.logistics.tracking")
	if !ok || !slices.Contains(c.Tags, "ulp:transport:a2a") || c.SpecURL != intents.DialectDCSATnT.SchemaURL {
		t.Errorf("tracking capability %+v, %v", c, ok)
	}
	if c, _ := profile([]string{intents.OceanTnTRequest}, "https://"+operatorDomain+"/a2a").Capability("dev.ulp.logistics.tracking"); slices.Contains(c.Tags, "ulp:transport:a2a") {
		t.Error("A2A tagged without the inquiry")
	}
	if c, _ := profile([]string{intents.TnTInquiry}, "").Capability("dev.ulp.logistics.tracking"); c.SpecURL != "" {
		t.Errorf("an inquiry-only node claims the status pull's schema: %+v", c)
	}
	if c, ok := profile([]string{intents.TnTEvent}, "").Capability("dev.ulp.logistics.tracking.subscriber"); !ok || !slices.Contains(c.Tags, "ulp:subscriber") {
		t.Errorf("subscriber capability %+v, %v", c, ok)
	}
}

func TestSlogObserver(t *testing.T) {
	var buf bytes.Buffer
	o := node.SlogObserver(slog.New(slog.NewJSONHandler(&buf, nil)))
	o(node.Event{Direction: "in", Intent: intents.BookingRequest, Sender: shipperDomain, Outcome: "rejected", Code: "stale"})
	o(node.Event{Direction: "in", Outcome: "accepted"})
	out := buf.String()
	if !strings.Contains(out, `"level":"WARN"`) || !strings.Contains(out, `"code":"stale"`) || !strings.Contains(out, `"level":"INFO"`) {
		t.Errorf("slog output: %s", out)
	}
}

func TestRejectionError(t *testing.T) {
	if s := node.Reject("forbidden", "Not allowed.", "deny list").Error(); !strings.Contains(s, "deny list") {
		t.Errorf("Error() = %q", s)
	}
	if s := (&node.Rejection{Code: "stale", Detail: "Old."}).Error(); s != "stale: Old." {
		t.Errorf("Error() = %q", s)
	}
	if s := (&node.RemoteError{Code: "forbidden", Detail: "No."}).Error(); !strings.Contains(s, "forbidden") {
		t.Errorf("Error() = %q", s)
	}
}

// TestNoMessagesToSelf: a node with several roles still never sends to
// itself, over any binding; nothing is posted.
func TestNoMessagesToSelf(t *testing.T) {
	op := newParty(t, "ob-2026-1")
	c := baseConfig(t, op, staticKeys{}, handlerFunc{})
	posted := false
	c.Poster = posterFunc(func(context.Context, string, []byte) ([]byte, error) { posted = true; return nil, nil })
	n, err := node.New(c)
	if err != nil {
		t.Fatal(err)
	}
	h := envelope.NewHeader(intents.BookingRequest, operatorDomain, operatorDomain, time.Now())
	if _, err := n.Exchange(context.Background(), h, bookingRequest(), intents.BookingConfirmation); !errors.Is(err, node.ErrSelf) {
		t.Errorf("Exchange to self: %v", err)
	}
	if _, err := n.ExchangeA2A(context.Background(), h, bookingRequest(), intents.BookingConfirmation); !errors.Is(err, node.ErrSelf) {
		t.Errorf("ExchangeA2A to self: %v", err)
	}
	if err := n.Push(context.Background(), h, bookingRequest(), "https://"+operatorDomain+"/ulp"); !errors.Is(err, node.ErrSelf) {
		t.Errorf("Push to self: %v", err)
	}
	if posted {
		t.Error("a message to self was posted")
	}
}

// TestRejectedResponseEvent: a response that fails its checks reaches the
// observer with its exact bytes and the header it claims, unverified and
// with no body, in the conversation this node opened.
func TestRejectedResponseEvent(t *testing.T) {
	op, mallory := newParty(t, "ob-2026-1"), newParty(t, "m-1")
	var resp []byte
	c := baseConfig(t, op, staticKeys{}, handlerFunc{})
	c.Domain, c.Roles = shipperDomain, []string{"shipper"}
	var events []node.Event
	c.Observer = func(e node.Event) { events = append(events, e) }
	c.Poster = posterFunc(func(context.Context, string, []byte) ([]byte, error) { return resp, nil })
	c.Handlers = nil
	n, err := node.New(c)
	if err != nil {
		t.Fatal(err)
	}
	h := envelope.NewHeader(intents.BookingRequest, shipperDomain, operatorDomain, time.Now())
	// From someone other than the receiver: refused before any key lookup.
	rh := h.Reply(intents.BookingConfirmation, time.Now())
	rh.Sender = "mallory.example"
	resp = seal(t, mallory.signer, "m-1", rh, map[string]any{"booking_reference": "X"})
	if _, err := n.Exchange(context.Background(), h, bookingRequest(), intents.BookingConfirmation); !errors.Is(err, node.ErrBadResponse) {
		t.Fatalf("Exchange: %v", err)
	}
	var in *node.Event
	for i := range events {
		if events[i].Direction == "in" {
			in = &events[i]
		}
	}
	if in == nil || in.Verified || in.Body != nil || string(in.Wire) != string(resp) || in.Header == nil ||
		in.Header.Sender != "mallory.example" || in.Opener != intents.BookingRequest || !in.OpenedHere {
		t.Errorf("rejected response event: %+v", in)
	}
	// The request went out with its bytes, signed by this node.
	if out := events[0]; out.Direction != "out" || out.Wire == nil || out.Body == nil || !out.OpenedHere || out.Opener != intents.BookingRequest {
		t.Errorf("request event: %+v", out)
	}
}
