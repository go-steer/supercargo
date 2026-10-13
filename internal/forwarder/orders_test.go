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

package forwarder

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

const (
	meridian = "meridian-forwarding.example"
	cymbal   = "cymbal-shoes.example"
)

type noKeys struct{}

func (noKeys) Key(context.Context, string, string) (sig.Verifier, error) {
	return nil, discovery.ErrUnknownKey
}

// sent records the answers the forwarder pushes; refuse makes the
// customer's node refuse them.
type sent struct {
	mu     sync.Mutex
	bodies []intents.ForwardingConfirmationBody
	heads  []envelope.Header
	refuse error
	key    sig.Verifier // the forwarder's: what the customer verifies with
}

func (s *sent) post(_ context.Context, _ string, msg []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refuse != nil {
		return nil, s.refuse
	}
	u, err := envelope.Parse(msg)
	if err != nil {
		return nil, err
	}
	e, err := u.Verify(s.key)
	if err != nil {
		return nil, err
	}
	var c intents.ForwardingConfirmationBody
	_ = json.Unmarshal(e.Body, &c)
	s.bodies, s.heads = append(s.bodies, c), append(s.heads, e.Header)
	return nil, rest.ErrNoResponse // a 204: accepted
}

type poster func(context.Context, string, []byte) ([]byte, error)

func (p poster) Post(ctx context.Context, r string, m []byte) ([]byte, error) { return p(ctx, r, m) }

func (p poster) PostTo(ctx context.Context, r, _ string, m []byte) ([]byte, error) {
	return p(ctx, r, m)
}

func desk(t *testing.T) (*Orders, *sent) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := sig.NewEd25519Signer(priv)
	jwk, _ := sig.Ed25519JWK(pub, "mf-1")
	key, _ := sig.NewEd25519Verifier(pub)
	out := &sent{key: key}
	n, err := node.New(node.Config{Domain: meridian, Name: "Meridian", Roles: []string{"operator", "shipper"},
		KID: "mf-1", Signer: signer, PublicJWK: jwk, Keys: noKeys{}, Replay: store.NewReplay(store.ReplayOptions{}),
		Poster: poster(out.post)})
	if err != nil {
		t.Fatal(err)
	}
	return &Orders{Node: n, Bookings: store.NewBookings(), RefPrefix: "MF", Deny: map[string]bool{"blocked.example": true}, Now: time.Now}, out
}

func anOrder() *intents.ForwardingRequestBody {
	return &intents.ForwardingRequestBody{
		ShipperRef: "CY-PO-55120",
		Incoterms:  intents.Incoterms{Rule: "DAP", Version: "2020", NamedPlace: "Cajamar"},
		Shipper:    intents.Party{Name: "Cymbal Shoes"}, Consignee: intents.Party{Name: "Cymbal Calçados"},
		PlaceOfReceipt:  intents.Place{Type: intents.PlacePort, Facility: "Euromax", UNLocationCode: "NLRTM"},
		PlaceOfDelivery: intents.Place{Type: intents.PlacePort, Facility: "Tecon", UNLocationCode: "BRSSZ", Latest: "2026-10-27T12:00:00Z"},
		Equipment:       []intents.Equipment{{ISOType: "45G1", Count: 1, GrossWeightKg: 12000}},
		Cargo:           intents.Cargo{Description: "Shoes", HSCode: "6404"},
		Subscription:    &intents.Subscription{Endpoint: "https://cymbal-shoes.example/ulp", Intents: []string{intents.TnTEvent}},
	}
}

func orderFrom(sender string, b *intents.ForwardingRequestBody) *node.Request {
	h := envelope.NewHeader(intents.ForwardingRequest, sender, meridian, time.Now())
	return &node.Request{Envelope: &envelope.Envelope{Header: h}, Body: b}
}

func aPlan() *intents.ForwardingConfirmationBody {
	return &intents.ForwardingConfirmationBody{Status: intents.StatusConfirmed, PlannedDelivery: "2026-10-23T08:00:00Z",
		Legs: []intents.PlannedLeg{{Mode: intents.ModeOcean, From: "NLRTM", To: "BRSSZ"}}}
}

func forbidden(t *testing.T, name string, err error) {
	t.Helper()
	var r *node.Rejection
	if !errors.As(err, &r) || r.Code != intents.CodeForbidden {
		t.Errorf("%s: %v, want a forbidden Rejection", name, err)
	}
}

// TestTakeOrder: an order is recorded under a new reference with the
// message it came in, and answered pending with its subscription.
func TestTakeOrder(t *testing.T) {
	o, _ := desk(t)
	var taken []store.Booking
	o.OnOrder = func(b store.Booking) { taken = append(taken, b) }
	req := orderFrom(cymbal, anOrder())
	if err := o.Authorize(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	reply, err := o.Handle(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	c := reply.Body.(*intents.ForwardingConfirmationBody)
	if reply.BookingRef != "MF-0007" || c.Status != intents.StatusPending || c.SubscriptionID != "SUB-MF-0007" || c.Validate() != nil {
		t.Fatalf("reply %s %+v", reply.BookingRef, c)
	}
	b, err := o.Bookings.Get(meridian, "MF-0007")
	if err != nil || b.Order == nil || b.Order.MessageID != req.Envelope.Header.MessageID || b.Shipper != cymbal || len(taken) != 1 {
		t.Fatalf("recorded %+v, %v", b, err)
	}
	unsubscribed := anOrder()
	unsubscribed.Subscription = nil
	reply, _ = o.Handle(context.Background(), orderFrom(cymbal, unsubscribed))
	if c := reply.Body.(*intents.ForwardingConfirmationBody); reply.BookingRef != "MF-0008" || c.SubscriptionID != "" {
		t.Errorf("second order: %s %+v", reply.BookingRef, c)
	}
	full := &Orders{Bookings: store.NewBookingsCap(0), RefPrefix: "MF", Now: time.Now}
	reply, err = full.Handle(context.Background(), orderFrom(cymbal, anOrder()))
	if err != nil || reply.Body.(*intents.ForwardingConfirmationBody).Status != intents.StatusRejected {
		t.Errorf("at capacity: %+v, %v", reply, err)
	}
}

// TestTakeOrderRefused: a denied sender, and a subscription to another
// domain, are refused before anything is recorded.
func TestTakeOrderRefused(t *testing.T) {
	o, _ := desk(t)
	forbidden(t, "denied", o.Authorize(context.Background(), orderFrom("blocked.example", anOrder())))
	elsewhere := anOrder()
	elsewhere.Subscription.Endpoint = "https://victim.example/ulp"
	forbidden(t, "subscription elsewhere", o.Authorize(context.Background(), orderFrom(cymbal, elsewhere)))
}

// TestAnswer: a pending order's answer goes to its shipper in reply to the
// order, keeps its subscription, and is recorded once; anything else is
// refused before it is sent.
func TestAnswer(t *testing.T) {
	o, out := desk(t)
	var answered []store.Booking
	o.OnAnswer = func(b store.Booking) { answered = append(answered, b) }
	req := orderFrom(cymbal, anOrder())
	if _, err := o.Handle(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		ref  string
		conf *intents.ForwardingConfirmationBody
		want error
	}{
		"an unknown order":    {"MF-9999", aPlan(), ErrUnknownOrder},
		"pending again":       {"MF-0007", &intents.ForwardingConfirmationBody{Status: intents.StatusPending}, ErrInvalidAnswer},
		"a malformed plan":    {"MF-0007", &intents.ForwardingConfirmationBody{Status: intents.StatusConfirmed}, ErrInvalidAnswer},
		"a plan ending short": {"MF-0007", func() *intents.ForwardingConfirmationBody { p := aPlan(); p.Legs[0].To = "BRRIG"; return p }(), ErrInvalidAnswer},
		"a plan delivered late": {"MF-0007", func() *intents.ForwardingConfirmationBody {
			p := aPlan()
			p.PlannedDelivery = "2026-10-28T08:00:00Z"
			return p
		}(), ErrInvalidAnswer},
	} {
		if err := o.Answer(context.Background(), tc.ref, tc.conf); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
	if len(out.bodies) != 0 {
		t.Fatalf("%d sent before a valid answer", len(out.bodies))
	}
	// The customer's node refusing it: nothing is recorded.
	out.refuse = errors.New("connection refused")
	if err := o.Answer(context.Background(), "MF-0007", aPlan()); err == nil {
		t.Error("a refused answer")
	}
	if b, _ := o.Bookings.Get(meridian, "MF-0007"); b.Order.Confirmation.Status != intents.StatusPending {
		t.Error("an answer not delivered was recorded")
	}
	out.refuse = nil
	if err := o.Answer(context.Background(), "MF-0007", aPlan()); err != nil {
		t.Fatal(err)
	}
	h, c := out.heads[0], out.bodies[0]
	if h.Receiver != cymbal || *h.InReplyTo != req.Envelope.Header.MessageID || *h.BookingRef != "MF-0007" ||
		c.OrderRef != "MF-0007" || c.SubscriptionID != "SUB-MF-0007" || c.Status != intents.StatusConfirmed {
		t.Errorf("sent %+v %+v", h, c)
	}
	if b, _ := o.Bookings.Get(meridian, "MF-0007"); b.Order.Confirmation.Status != intents.StatusConfirmed || len(answered) != 1 {
		t.Errorf("recorded %+v", b.Order.Confirmation)
	}
	if err := o.Answer(context.Background(), "MF-0007", aPlan()); !errors.Is(err, ErrAnswered) {
		t.Errorf("a second answer: %v", err)
	}
	// A rejection is a final answer too, without a subscription.
	if _, err := o.Handle(context.Background(), orderFrom(cymbal, anOrder())); err != nil {
		t.Fatal(err)
	}
	if err := o.Answer(context.Background(), "MF-0008", &intents.ForwardingConfirmationBody{Status: intents.StatusRejected}); err != nil {
		t.Errorf("a rejection: %v", err)
	}
}

// TestAnswerOneAtATime: two answers to one order sent at once: only one
// goes out.
func TestAnswerOneAtATime(t *testing.T) {
	o, out := desk(t)
	if _, err := o.Handle(context.Background(), orderFrom(cymbal, anOrder())); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- o.Answer(context.Background(), "MF-0007", aPlan())
		}()
	}
	wg.Wait()
	close(errs)
	ok := 0
	for err := range errs {
		if err == nil {
			ok++
		} else if !errors.Is(err, ErrAnswered) {
			t.Error(err)
		}
	}
	if ok != 1 || len(out.bodies) != 1 {
		t.Errorf("%d answers accepted, %d sent", ok, len(out.bodies))
	}
}

// TestControlAnswer: POST /orders/answer answers an order, with the
// outcome's status code.
func TestControlAnswer(t *testing.T) {
	o, _ := desk(t)
	if _, err := o.Handle(context.Background(), orderFrom(cymbal, anOrder())); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	RegisterControl(mux, o, nil)
	post := func(body string) int {
		req := httptest.NewRequest(http.MethodPost, "/orders/answer", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}
	plan, _ := json.Marshal(aPlan())
	// In order: the last two answer the same order twice.
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"malformed", `{"orderRef": "MF-0007", "x": 1}`, http.StatusBadRequest},
		{"an unknown order", `{"orderRef": "MF-9999", "answer": ` + string(plan) + `}`, http.StatusNotFound},
		{"invalid", `{"orderRef": "MF-0007", "answer": {"status": "pending"}}`, http.StatusBadRequest},
		{"the answer", `{"orderRef": "MF-0007", "answer": ` + string(plan) + `}`, http.StatusOK},
		{"again", `{"orderRef": "MF-0007", "answer": ` + string(plan) + `}`, http.StatusConflict},
	} {
		if code := post(tc.body); code != tc.want {
			t.Errorf("%s: HTTP %d, want %d", tc.name, code, tc.want)
		}
	}
}
