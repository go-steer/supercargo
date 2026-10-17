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

package operator

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/delegation"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
	"github.com/go-steer/supercargo/pkg/sig"
)

const dc = "cymbal-footwear.example"

var at = time.Date(2026, 10, 9, 15, 0, 0, 0, time.UTC)

// mint is a grant from issuer to grantee for ref with ocean-blue.example,
// signed and verified as the pipeline does.
func mint(t *testing.T, issuer, grantee, ref string, scope ...string) *delegation.Verified {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	s, _ := sig.NewEd25519Signer(priv)
	v, _ := sig.NewEd25519Verifier(pub)
	g, err := delegation.New(issuer, "ocean-blue.example", grantee, ref, scope, at, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := delegation.Seal(g, "k-1", s)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := delegation.Parse(wire)
	ver, err := u.Verify(context.Background(), func(context.Context, string, string) (sig.Verifier, error) { return v, nil })
	if err != nil {
		t.Fatal(err)
	}
	return ver
}

func delegatedReq(intent, sender, ref string, body intents.Body, d *delegation.Verified, now time.Time) *node.Request {
	h := envelope.NewHeader(intent, sender, "ocean-blue.example", now)
	h.BookingRef = &ref
	return node.NewRequest(&envelope.Envelope{Header: h}, body, d, now, 5*time.Minute)
}

// TestDelegatedRead: a company the booking's shipper delegated tnt:read
// to may ask; a grant from anyone else, for another booking, without the
// scope, or expired, gets the one refusal.
func TestDelegatedRead(t *testing.T) {
	f := newInquiryFixture(t, nil)
	q := &intents.TnTInquiryBody{Question: "When?"}
	ok := mint(t, "cymbal-shoes.example", dc, "OB-BKG-1", delegation.ScopeRead)
	if err := f.svc.Authorize(context.Background(), delegatedReq(intents.TnTInquiry, dc, "OB-BKG-1", q, ok, at.Add(time.Hour))); err != nil {
		t.Errorf("the delegate: %v", err)
	}
	st := &StatusService{Bookings: f.svc.Bookings, Log: f.svc.Log}
	if err := st.Authorize(context.Background(), delegatedReq(intents.OceanTnTRequest, dc, "OB-BKG-1", nil, ok, at.Add(time.Hour))); err != nil {
		t.Errorf("the delegate's status request: %v", err)
	}
	for name, req := range map[string]*node.Request{
		"not the shipper's grant": delegatedReq(intents.TnTInquiry, dc, "OB-BKG-1", q, mint(t, "mallory.example", dc, "OB-BKG-1", delegation.ScopeRead), at.Add(time.Hour)),
		"another shipper's booking": delegatedReq(intents.TnTInquiry, dc, "OB-BKG-2", q,
			mint(t, "cymbal-shoes.example", dc, "OB-BKG-2", delegation.ScopeRead), at.Add(time.Hour)),
		"granted another booking": delegatedReq(intents.TnTInquiry, dc, "OB-BKG-1", q, mint(t, "cymbal-shoes.example", dc, "OB-BKG-9", delegation.ScopeRead), at.Add(time.Hour)),
		"subscribe only":          delegatedReq(intents.TnTInquiry, dc, "OB-BKG-1", q, mint(t, "cymbal-shoes.example", dc, "OB-BKG-1", delegation.ScopeSubscribe), at.Add(time.Hour)),
		"sent by someone else":    delegatedReq(intents.TnTInquiry, "mallory.example", "OB-BKG-1", q, ok, at.Add(time.Hour)),
		"expired":                 delegatedReq(intents.TnTInquiry, dc, "OB-BKG-1", q, ok, at.Add(8*24*time.Hour)),
		"an unknown booking":      delegatedReq(intents.TnTInquiry, dc, "OB-BKG-404", q, ok, at.Add(time.Hour)),
	} {
		var rej *node.Rejection
		if err := f.svc.Authorize(context.Background(), req); !errors.As(err, &rej) || rej.Detail != notAParty.Detail {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func subReq(sender, ref, endpoint string, d *delegation.Verified, now time.Time) *node.Request {
	return delegatedReq(intents.TnTSubscriptionRequest, sender, ref,
		&intents.TnTSubscriptionRequestBody{Subscription: intents.Subscription{Endpoint: endpoint, Intents: []string{intents.TnTEvent, intents.TnTAlert}}}, d, now)
}

// TestSubscription: the delegate subscribes its own endpoint until the
// grant ends, and is answered with the leg; every other way is refused.
func TestSubscription(t *testing.T) {
	f := newInquiryFixture(t, nil)
	var subscribed []string
	s := &SubscriptionService{Bookings: f.svc.Bookings, Now: func() time.Time { return at.Add(time.Hour) },
		OnSubscribed: func(_ store.Booking, d store.Delegate) { subscribed = append(subscribed, d.Grantee) }}
	d := mint(t, "cymbal-shoes.example", dc, "OB-BKG-1", delegation.ScopeRead, delegation.ScopeSubscribe)
	req := subReq(dc, "OB-BKG-1", "https://"+dc+"/ulp", d, at.Add(time.Hour))
	if err := s.Authorize(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	reply, err := s.Handle(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	c := reply.Body.(*intents.TnTSubscriptionConfirmationBody)
	if err := c.Validate(); err != nil || c.Status != intents.StatusConfirmed || c.Leg.From != "NLRTM" || c.ExpiresAt != "2026-10-16T15:00:00Z" {
		t.Fatalf("%+v %v", c, err)
	}
	b, _ := f.svc.Bookings.Get("ocean-blue.example", "OB-BKG-1")
	if len(b.Delegates) != 1 || b.Delegates[0].TokenID != d.TokenID() || len(subscribed) != 1 {
		t.Fatalf("%+v", b.Delegates)
	}
	// Again: replaced, not added.
	if _, err := s.Handle(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if b, _ := f.svc.Bookings.Get("ocean-blue.example", "OB-BKG-1"); len(b.Delegates) != 1 {
		t.Errorf("subscribed twice: %+v", b.Delegates)
	}
	for name, req := range map[string]*node.Request{
		"no delegation":           subReq(dc, "OB-BKG-1", "https://"+dc+"/ulp", nil, at.Add(time.Hour)),
		"read only":               subReq(dc, "OB-BKG-1", "https://"+dc+"/ulp", mint(t, "cymbal-shoes.example", dc, "OB-BKG-1", delegation.ScopeRead), at.Add(time.Hour)),
		"not the shipper's":       subReq(dc, "OB-BKG-1", "https://"+dc+"/ulp", mint(t, "mallory.example", dc, "OB-BKG-1", delegation.ScopeSubscribe), at.Add(time.Hour)),
		"an unknown booking":      subReq(dc, "OB-BKG-404", "https://"+dc+"/ulp", d, at.Add(time.Hour)),
		"expired":                 subReq(dc, "OB-BKG-1", "https://"+dc+"/ulp", d, at.Add(8*24*time.Hour)),
		"someone else's endpoint": subReq(dc, "OB-BKG-1", "https://evil.example/ulp", d, at.Add(time.Hour)),
	} {
		var rej *node.Rejection
		if err := s.Authorize(context.Background(), req); !errors.As(err, &rej) || rej.Code != intents.CodeForbidden {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A cancelled booking takes no subscription.
	_, _ = f.svc.Bookings.Update("ocean-blue.example", "OB-BKG-1", func(b *store.Booking) error { b.CancelledAt = at; return nil })
	if err := s.Authorize(context.Background(), req); err == nil {
		t.Error("subscribed to a cancelled booking")
	}
}

// TestSubscriptionFull: a booking keeps at most MaxDelegates; an expired
// one makes room.
func TestSubscriptionFull(t *testing.T) {
	f := newInquiryFixture(t, nil)
	now := at.Add(time.Hour)
	s := &SubscriptionService{Bookings: f.svc.Bookings, Now: func() time.Time { return now }, MaxDelegates: 2}
	for i := range 3 {
		grantee := fmt.Sprintf("dc%d.example", i)
		r, err := s.Handle(context.Background(), subReq(grantee, "OB-BKG-1", "https://"+grantee+"/ulp",
			mint(t, "cymbal-shoes.example", grantee, "OB-BKG-1", delegation.ScopeSubscribe), now))
		if err != nil {
			t.Fatal(err)
		}
		want := intents.StatusConfirmed
		if i == 2 {
			want = intents.StatusRejected
		}
		if c := r.Body.(*intents.TnTSubscriptionConfirmationBody); c.Status != want || c.Validate() != nil {
			t.Errorf("subscription %d: %+v", i, c)
		}
	}
	now = at.Add(8 * 24 * time.Hour) // the two expired
	r, _ := s.Handle(context.Background(), subReq("dc3.example", "OB-BKG-1", "https://dc3.example/ulp",
		mint(t, "cymbal-shoes.example", "dc3.example", "OB-BKG-1", delegation.ScopeSubscribe), at.Add(time.Hour)))
	if c := r.Body.(*intents.TnTSubscriptionConfirmationBody); c.Status != intents.StatusConfirmed {
		t.Errorf("after expiry: %+v", c)
	}
}

// pushes records what was pushed where.
type pushes struct {
	mu  sync.Mutex
	got []string
}

func (p *pushes) Push(_ context.Context, h envelope.Header, _ intents.Body, endpoint string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.got = append(p.got, h.Receiver+" "+endpoint)
	return nil
}

func (p *pushes) list() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.got...)
}

// TestPublishToDelegates: an event goes to the shipper's subscription and
// to each delegate's still in force, each to its own endpoint.
func TestPublishToDelegates(t *testing.T) {
	now := at.Add(time.Hour)
	p := &pushes{}
	pub := &Publisher{Pusher: p, Domain: "ocean-blue.example", Log: store.NewEventLog(0), Now: func() time.Time { return now }}
	defer pub.Close()
	b := store.Booking{Ref: "OB-BKG-1", Shipper: "cymbal-shoes.example", Operator: "ocean-blue.example", Request: *body(),
		Delegates: []store.Delegate{
			{Grantee: dc, Subscription: intents.Subscription{Endpoint: "https://" + dc + "/ulp", Intents: []string{intents.TnTEvent}}, Expires: at.Add(24 * time.Hour)},
			{Grantee: "old.example", Subscription: intents.Subscription{Endpoint: "https://old.example/ulp", Intents: []string{intents.TnTEvent}}, Expires: at},
			{Grantee: "alerts.example", Subscription: intents.Subscription{Endpoint: "https://alerts.example/ulp", Intents: []string{intents.TnTAlert}}, Expires: at.Add(24 * time.Hour)},
		}}
	b.Request.Subscription = &intents.Subscription{Endpoint: "https://cymbal-shoes.example/ulp", Intents: []string{intents.TnTEvent, intents.TnTAlert}}
	ev := &intents.TnTEventBody{EventID: "EV-1", EventType: intents.EventTypeTransport, EventClassifierCode: intents.ClassifierActual,
		EventTypeCode: "DEPA", EventDateTime: "2026-10-09T15:00:00Z", UNLocationCode: "NLRTM", FacilityName: "Rotterdam", Description: "Departed"}
	if err := pub.Publish(b, ev); err != nil {
		t.Fatal(err)
	}
	for range 100 {
		if len(p.list()) >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	got := map[string]bool{}
	for _, g := range p.list() {
		got[g] = true
	}
	if len(got) != 2 || !got["cymbal-shoes.example "+b.Request.Subscription.Endpoint] || !got[dc+" https://"+dc+"/ulp"] {
		t.Errorf("pushed %v", p.list())
	}
}
