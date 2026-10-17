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

package shipper

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/delegation"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
	"github.com/go-steer/supercargo/pkg/sig"
)

const (
	issuerDomain = "vale-verde.example"
	carrier      = "ocean-blue.example"
)

// tokenOf is the delegation attached to msg, as sent.
func tokenOf(t *testing.T, msg []byte) json.RawMessage {
	t.Helper()
	var w struct{ Payload string }
	_ = json.Unmarshal(msg, &w)
	raw, _ := sig.DecodeBase64URL(w.Payload)
	var env envelope.Envelope
	_ = json.Unmarshal(raw, &env)
	return env.Attachments.Delegation
}

// grantShipper holds OB-BKG-1 as its shipper with ocean-blue.example; its
// node's outbound messages go to last, answered by answer.
func grantShipper(t *testing.T, last *[]byte, answer func(envelope.Header) (string, any)) *Shipper {
	t.Helper()
	s := shipperWith(t, func(h envelope.Header) (string, any) { return answer(h) }, func(c *node.Config) {
		inner := c.Poster
		c.Poster = poster(func(ctx context.Context, r string, msg []byte) ([]byte, error) {
			*last = append([]byte(nil), msg...)
			return inner.Post(ctx, r, msg)
		})
	})
	if err := s.Bookings.Add(store.Booking{Ref: "OB-BKG-1", Operator: carrier, Shipper: "cymbal-shoes.example"}); err != nil {
		t.Fatal(err)
	}
	return s
}

func silent(envelope.Header) (string, any) { return "", nil }

// TestGrant: the shipper hands its booking's grant to the grantee, signed,
// and records it; what isn't its booking, or isn't a valid grant, isn't.
func TestGrant(t *testing.T) {
	var last []byte
	s := grantShipper(t, &last, silent)
	g, err := s.Grant(context.Background(), carrier, "OB-BKG-1", "aurora.example", []string{delegation.ScopeSubscribe, delegation.ScopeRead}, 7*24*time.Hour, "Your delivery")
	if err != nil {
		t.Fatal(err)
	}
	u, err := delegation.Parse(tokenOf(t, last))
	if err != nil || u.Issuer() != "cymbal-shoes.example" {
		t.Fatalf("the token sent: %v", err)
	}
	b, _ := s.Bookings.Get(carrier, "OB-BKG-1")
	if len(b.Grants) != 1 || !reflect.DeepEqual(b.Grants[0], *g) || g.Grantee != "aurora.example" || g.Note != "Your delivery" {
		t.Errorf("recorded %+v", b.Grants)
	}
	for name, try := range map[string]func() error{
		"not held": func() error {
			_, err := s.Grant(context.Background(), carrier, "OB-BKG-9", "aurora.example", []string{delegation.ScopeRead}, time.Hour, "")
			return expect(err, ErrNotGrantable)
		},
		"an unknown scope": func() error {
			_, err := s.Grant(context.Background(), carrier, "OB-BKG-1", "aurora.example", []string{"tnt:write"}, time.Hour, "")
			return expect(err, delegation.ErrMalformed)
		},
		"a long note": func() error {
			_, err := s.Grant(context.Background(), carrier, "OB-BKG-1", "aurora.example", []string{delegation.ScopeRead}, time.Hour, strings.Repeat("x", 281))
			return expectAny(err)
		},
	} {
		if err := try(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A booking we follow by someone else's grant, or one cancelled, isn't
	// ours to grant.
	_ = s.Bookings.Add(store.Booking{Ref: "OB-BKG-2", Operator: carrier, Shipper: issuerDomain, Delegated: &store.DelegatedBooking{Issuer: issuerDomain}})
	if _, err := s.Grant(context.Background(), carrier, "OB-BKG-2", "aurora.example", []string{delegation.ScopeRead}, time.Hour, ""); !errors.Is(err, ErrNotGrantable) {
		t.Errorf("a delegated booking: %v", err)
	}
	_, _ = s.Bookings.Update(carrier, "OB-BKG-1", func(b *store.Booking) error { b.CancelledAt = time.Now(); return nil })
	if _, err := s.Grant(context.Background(), carrier, "OB-BKG-1", "aurora.example", []string{delegation.ScopeRead}, time.Hour, ""); !errors.Is(err, ErrNotGrantable) {
		t.Errorf("a cancelled booking: %v", err)
	}
}

func expect(err, want error) error {
	if !errors.Is(err, want) {
		return errors.New("got " + errString(err) + ", want " + want.Error())
	}
	return nil
}

func expectAny(err error) error {
	if err == nil {
		return errors.New("no error")
	}
	return nil
}

func errString(err error) string {
	if err == nil {
		return "nil"
	}
	return err.Error()
}

// mintFor is a grant from issuer to grantee for ref with carrier,
// verified as the pipeline does.
func mintFor(t *testing.T, issuer, grantee, ref string, now time.Time, scope ...string) (*delegation.Verified, []byte) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	s, _ := sig.NewEd25519Signer(priv)
	v, _ := sig.NewEd25519Verifier(pub)
	g, err := delegation.New(issuer, carrier, grantee, ref, scope, now, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	wire, _ := delegation.Seal(g, "k-1", s)
	u, _ := delegation.Parse(wire)
	ver, err := u.Verify(context.Background(), func(context.Context, string, string) (sig.Verifier, error) { return v, nil })
	if err != nil {
		t.Fatal(err)
	}
	return ver, wire
}

func grantReq(sender, ref string, d *delegation.Verified, wire []byte, now time.Time) *node.Request {
	h := envelope.NewHeader(intents.DelegationGrant, sender, "cymbal-shoes.example", now)
	h.BookingRef = &ref
	env := &envelope.Envelope{Header: h, Attachments: envelope.Attachments{Delegation: wire}}
	return node.NewRequest(env, &intents.DelegationGrantBody{Note: "For your DC"}, d, now, 5*time.Minute)
}

// TestGrantsHandler: a grant handed to us by its issuer is kept as a
// delegated booking with its carrier, and renewed by the next; one not
// issued by its sender, without a delegation, or over a booking that's
// ours, is refused.
func TestGrantsHandler(t *testing.T) {
	now := time.Now()
	s := shipperWith(t, silent)
	var granted []store.Booking
	g := &Grants{Shipper: s, Endpoint: "https://cymbal-shoes.example/ulp", From: map[string]bool{issuerDomain: true, "mallory.example": true},
		OnGranted: func(b store.Booking) { granted = append(granted, b) }}
	d, wire := mintFor(t, issuerDomain, "cymbal-shoes.example", "OB-BKG-7", now, delegation.ScopeRead, delegation.ScopeSubscribe)
	req := grantReq(issuerDomain, "OB-BKG-7", d, wire, now)
	if err := g.Authorize(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Handle(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	b, err := s.Bookings.Get(carrier, "OB-BKG-7")
	if err != nil || b.Delegated == nil || b.Shipper != issuerDomain || b.Delegated.TokenID != d.TokenID() || b.Delegated.Note != "For your DC" ||
		string(b.Delegated.Token) != string(wire) || len(granted) != 1 {
		t.Fatalf("%+v %v", b, err)
	}
	// Renewed: the subscription kept, the token the new one.
	_, _ = s.Bookings.Update(carrier, "OB-BKG-7", func(b *store.Booking) error { b.Delegated.SubscriptionID = "SUB-1"; return nil })
	d2, wire2 := mintFor(t, issuerDomain, "cymbal-shoes.example", "OB-BKG-7", now, delegation.ScopeRead)
	if _, err := g.Handle(context.Background(), grantReq(issuerDomain, "OB-BKG-7", d2, wire2, now)); err != nil {
		t.Fatal(err)
	}
	if b, _ := s.Bookings.Get(carrier, "OB-BKG-7"); b.Delegated.TokenID != d2.TokenID() || b.Delegated.SubscriptionID != "SUB-1" {
		t.Errorf("renewed %+v", b.Delegated)
	}
	_ = s.Bookings.Add(store.Booking{Ref: "OB-BKG-8", Operator: carrier, Shipper: "cymbal-shoes.example"})
	ours, oursWire := mintFor(t, issuerDomain, "cymbal-shoes.example", "OB-BKG-8", now, delegation.ScopeRead)
	// Mallory, a listed issuer, would take over the booking Vale Verde
	// granted us; Stranger isn't listed at all.
	hijack, hijackWire := mintFor(t, "mallory.example", "cymbal-shoes.example", "OB-BKG-7", now, delegation.ScopeRead)
	stranger, strangerWire := mintFor(t, "stranger.example", "cymbal-shoes.example", "OB-BKG-5", now, delegation.ScopeRead)
	for name, req := range map[string]*node.Request{
		"no delegation":            grantReq(issuerDomain, "OB-BKG-7", nil, nil, now),
		"not issued by its sender": grantReq("mallory.example", "OB-BKG-7", d, wire, now),
		"for another booking":      grantReq(issuerDomain, "OB-BKG-9", d, wire, now),
		"expired":                  grantReq(issuerDomain, "OB-BKG-7", d, wire, now.Add(8*24*time.Hour)),
		"over a booking of ours":   grantReq(issuerDomain, "OB-BKG-8", ours, oursWire, now),
		"taking over another's":    grantReq("mallory.example", "OB-BKG-7", hijack, hijackWire, now),
		"an issuer not listed":     grantReq("stranger.example", "OB-BKG-5", stranger, strangerWire, now),
	} {
		var rej *node.Rejection
		if err := g.Authorize(context.Background(), req); !errors.As(err, &rej) || rej.Code != intents.CodeForbidden {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Ours meanwhile (between Authorize and Handle): not overwritten.
	_, _ = s.Bookings.Update(carrier, "OB-BKG-7", func(b *store.Booking) error { b.Delegated = nil; return nil })
	if _, err := g.Handle(context.Background(), grantReq(issuerDomain, "OB-BKG-7", d2, wire2, now)); err == nil {
		t.Error("a grant overwrote our own booking")
	}
}

// delegatedShipper follows OB-BKG-7 by issuerDomain's grant (scope),
// its carrier answering a subscription with answer.
func delegatedShipper(t *testing.T, last *[]byte, now time.Time, answer func(envelope.Header) (string, any), scope ...string) *Shipper {
	t.Helper()
	s := grantShipper(t, last, answer)
	d, wire := mintFor(t, issuerDomain, "cymbal-shoes.example", "OB-BKG-7", now, scope...)
	if err := s.Bookings.Add(store.Booking{Ref: "OB-BKG-7", Operator: carrier, Shipper: issuerDomain, Delegated: &store.DelegatedBooking{
		Issuer: issuerDomain, Token: wire, TokenID: d.TokenID(), Scope: d.Scope(), Expires: d.Expires()}}); err != nil {
		t.Fatal(err)
	}
	return s
}

func confirmed(leg intents.PlannedLeg) func(envelope.Header) (string, any) {
	return func(envelope.Header) (string, any) {
		return intents.TnTSubscriptionConfirmation, &intents.TnTSubscriptionConfirmationBody{Status: intents.StatusConfirmed,
			SubscriptionID: "SUB-D-1", ExpiresAt: "2030-01-01T00:00:00Z", Leg: &leg}
	}
}

// TestSubscribe: by the grant, the carrier confirms and the leg is
// recorded with our own subscription (a truck's, or a ship's); a refusal
// is returned; what the grant doesn't allow, or an answer that doesn't
// fit, isn't recorded.
func TestSubscribe(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	var last []byte
	road := intents.PlannedLeg{Mode: intents.ModeRoad, From: "BRSAO", To: "BRGRU", PlannedDeparture: "2026-10-20T08:00:00Z", PlannedArrival: "2026-10-21T14:00:00Z"}
	s := delegatedShipper(t, &last, now, confirmed(road), delegation.ScopeSubscribe)
	c, err := s.Subscribe(ctx, carrier, "OB-BKG-7", "https://cymbal-shoes.example/ulp")
	if err != nil || c.Status != intents.StatusConfirmed {
		t.Fatalf("%+v %v", c, err)
	}
	if len(tokenOf(t, last)) == 0 {
		t.Error("the grant didn't go with the request")
	}
	b, _ := s.Bookings.Get(carrier, "OB-BKG-7")
	if b.Delegated.SubscriptionID != "SUB-D-1" || b.Road == nil || b.Road.Request.Delivery.UNLocationCode != "BRGRU" ||
		b.Subscription().Endpoint != "https://cymbal-shoes.example/ulp" || b.Road.Confirmation.PlannedDelivery != road.PlannedArrival {
		t.Errorf("recorded %+v", b)
	}
	// A ship's leg goes where an ocean booking's does.
	ocean := intents.PlannedLeg{Mode: intents.ModeOcean, From: "NLRTM", To: "BRSSZ"}
	s2 := delegatedShipper(t, &last, now, confirmed(ocean), delegation.ScopeSubscribe)
	if _, err := s2.Subscribe(ctx, carrier, "OB-BKG-7", "https://cymbal-shoes.example/ulp"); err != nil {
		t.Fatal(err)
	}
	if b, _ := s2.Bookings.Get(carrier, "OB-BKG-7"); b.Request.PortOfDischarge != "BRSSZ" || b.Subscription() == nil {
		t.Errorf("an ocean leg %+v", b)
	}
	// Refused by the carrier: returned, nothing recorded.
	s3 := delegatedShipper(t, &last, now, func(envelope.Header) (string, any) {
		return intents.TnTSubscriptionConfirmation, &intents.TnTSubscriptionConfirmationBody{Status: intents.StatusRejected,
			Messages: []intents.Message{{Code: "too_many_subscriptions", Detail: "Full."}}}
	}, delegation.ScopeSubscribe)
	if c, err := s3.Subscribe(ctx, carrier, "OB-BKG-7", "https://cymbal-shoes.example/ulp"); err != nil || c.Status != intents.StatusRejected {
		t.Errorf("a refusal: %+v %v", c, err)
	}
	// What the grant doesn't allow, or isn't one.
	readOnly := delegatedShipper(t, &last, now, confirmed(road), delegation.ScopeRead)
	expired := delegatedShipper(t, &last, now.Add(-8*24*time.Hour), confirmed(road), delegation.ScopeSubscribe)
	nowhere := delegatedShipper(t, &last, now, confirmed(intents.PlannedLeg{Mode: intents.ModeRoad, From: "BRSAO", To: "BRSAO"}), delegation.ScopeSubscribe)
	for name, tc := range map[string]struct {
		s   *Shipper
		ref string
	}{
		"not delegated": {s, "OB-BKG-1"}, "read only": {readOnly, "OB-BKG-7"}, "expired": {expired, "OB-BKG-7"}, "a leg going nowhere": {nowhere, "OB-BKG-7"},
	} {
		if _, err := tc.s.Subscribe(ctx, carrier, tc.ref, "https://cymbal-shoes.example/ulp"); err == nil {
			t.Errorf("%s: subscribed", name)
		}
	}
}

// TestDelegatedQuestions: a question about a delegated booking carries
// the grant; an expired grant asks nothing; a status request isn't for it.
func TestDelegatedQuestions(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	var last []byte
	answer := func(h envelope.Header) (string, any) {
		return intents.TnTInquiryResponse, &intents.TnTInquiryResponseBody{Answer: "Tomorrow.", GeneratedBy: intents.GeneratedByTemplate,
			DataScope: intents.DataScope{BookingRef: *h.BookingRef}}
	}
	s := delegatedShipper(t, &last, now, answer, delegation.ScopeRead)
	if a, err := s.Ask(ctx, carrier, "OB-BKG-7", "When?"); err != nil || a.Response.Answer != "Tomorrow." || len(tokenOf(t, last)) == 0 {
		t.Errorf("asked %+v %v", a, err)
	}
	if _, err := s.Status(ctx, carrier, "OB-BKG-7"); err == nil || !strings.Contains(err.Error(), "delegation") {
		t.Errorf("status: %v", err)
	}
	old := delegatedShipper(t, &last, now.Add(-8*24*time.Hour), answer, delegation.ScopeRead)
	if _, err := old.Ask(ctx, carrier, "OB-BKG-7", "When?"); !errors.Is(err, ErrGrantExpired) {
		t.Errorf("expired: %v", err)
	}
	// Its pushes stop with the grant.
	in := &Inbound{Bookings: old.Bookings, Log: store.NewEventLog(0), Now: time.Now}
	_, _ = old.Bookings.Update(carrier, "OB-BKG-7", func(b *store.Booking) error {
		legInto(b, intents.PlannedLeg{Mode: intents.ModeRoad, From: "BRSAO", To: "BRGRU"}, intents.Subscription{Endpoint: "https://cymbal-shoes.example/ulp", Intents: []string{intents.TnTEvent}})
		return nil
	})
	ref := "OB-BKG-7"
	h := envelope.NewHeader(intents.TnTEvent, carrier, "cymbal-shoes.example", now)
	h.BookingRef = &ref
	if err := in.Authorize(ctx, node.NewRequest(&envelope.Envelope{Header: h}, nil, nil, now, time.Minute)); err == nil {
		t.Error("a push after the grant expired")
	}
}

// TestControlGrant: the routes' answers.
func TestControlGrant(t *testing.T) {
	var last []byte
	s := grantShipper(t, &last, silent)
	s.Endpoint = "https://cymbal-shoes.example/ulp"
	h := ControlHandler(s, nil)
	post := func(path, body string) int {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	for _, tc := range []struct {
		path, body string
		want       int
	}{
		{"/shipments/ocean-blue.example/OB-BKG-1/grant", `{"grantee":"aurora.example","scope":["tnt:read"],"days":7}`, 200},
		{"/shipments/ocean-blue.example/OB-BKG-9/grant", `{"grantee":"aurora.example","scope":["tnt:read"],"days":7}`, 404},
		{"/shipments/ocean-blue.example/OB-BKG-1/grant", `{"grantee":"aurora.example","scope":["tnt:write"],"days":7}`, 400},
		{"/shipments/ocean-blue.example/OB-BKG-1/grant", `{"grantee":"Aurora","scope":["tnt:read"],"days":7}`, 400},
		{"/shipments/ocean-blue.example/OB-BKG-1/grant", `{"grantee":"aurora.example","scope":["tnt:read"],"days":40}`, 400},
		{"/shipments/ocean-blue.example/OB-BKG-1/grant", `{`, 400},
		{"/shipments/ocean-blue.example/OB-BKG-1/subscribe", `{}`, 404},
	} {
		if got := post(tc.path, tc.body); got != tc.want {
			t.Errorf("%s %s: HTTP %d, want %d", tc.path, tc.body, got, tc.want)
		}
	}
	// A grantee whose node can't be reached.
	s2 := shipperWith(t, func(envelope.Header) (string, any) {
		return intents.Error, &intents.ErrorBody{Code: intents.CodeForbidden, Detail: "No."}
	})
	_ = s2.Bookings.Add(store.Booking{Ref: "OB-BKG-1", Operator: carrier, Shipper: "cymbal-shoes.example"})
	r := httptest.NewRequest(http.MethodPost, "/shipments/ocean-blue.example/OB-BKG-1/grant", strings.NewReader(`{"grantee":"aurora.example","scope":["tnt:read"],"days":7}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	ControlHandler(s2, nil).ServeHTTP(w, r)
	if w.Code != http.StatusBadGateway {
		t.Errorf("a refused grant: HTTP %d", w.Code)
	}
	// Subscribing: an expired grant, a carrier refusing with an error.
	var l2 []byte
	old := delegatedShipper(t, &l2, time.Now().Add(-8*24*time.Hour), silent, delegation.ScopeSubscribe)
	old.Endpoint = "https://cymbal-shoes.example/ulp"
	sub := func(s *Shipper) int {
		r := httptest.NewRequest(http.MethodPost, "/shipments/ocean-blue.example/OB-BKG-7/subscribe", strings.NewReader(`{}`))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		ControlHandler(s, nil).ServeHTTP(w, r)
		return w.Code
	}
	if got := sub(old); got != http.StatusConflict {
		t.Errorf("expired: HTTP %d", got)
	}
	failing := delegatedShipper(t, &l2, time.Now(), func(envelope.Header) (string, any) {
		return intents.Error, &intents.ErrorBody{Code: intents.CodeForbidden, Detail: "No."}
	}, delegation.ScopeSubscribe)
	failing.Endpoint = "https://cymbal-shoes.example/ulp"
	if got := sub(failing); got != http.StatusBadGateway {
		t.Errorf("refused: HTTP %d", got)
	}
	ok := delegatedShipper(t, &l2, time.Now(), confirmed(intents.PlannedLeg{Mode: intents.ModeRoad, From: "BRSAO", To: "BRGRU"}), delegation.ScopeSubscribe)
	ok.Endpoint = "https://cymbal-shoes.example/ulp"
	if got := sub(ok); got != http.StatusOK {
		t.Errorf("subscribed: HTTP %d", got)
	}
}
