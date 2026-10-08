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
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
	"github.com/go-steer/supercargo/pkg/sig"
)

func bookingRequest() *intents.BookingRequestBody {
	return &intents.BookingRequestBody{
		ShipperRef:        "CY-PO-55120",
		ShipperName:       "Cymbal Shoes",
		PortOfLoading:     "NLRTM",
		PortOfDischarge:   "USEWR",
		Equipment:         []intents.Equipment{{ISOType: "45G1", Count: 1, GrossWeightKg: 12000}},
		Cargo:             intents.Cargo{Description: "Footwear (sneakers)", HSCode: "6404"},
		EarliestDeparture: "2026-10-06",
		Consignee:         &intents.Consignee{Name: "Cymbal Footwear", Domain: "cymbal-footwear.example"},
		Subscription: &intents.Subscription{
			Endpoint: "https://cymbal-shoes.example/ulp",
			Intents:  []string{"ulp.tnt.event", "ulp.tnt.alert"},
		},
	}
}

// TestBookingOverREST is milestone 2's "done when": two nodes exchange a
// verified booking.request and booking.confirmation over REST, over TLS,
// each having fetched the other's profile and keys.
func TestBookingOverREST(t *testing.T) {
	d := newDemo(t)
	conf, err := d.ship.Book(context.Background(), operatorDomain, bookingRequest())
	if err != nil {
		t.Fatalf("Book: %v", err)
	}
	if conf.Status != intents.StatusConfirmed || conf.BookingRef != "OB-BKG-1042" || conf.EBL.VoyageNumber != "42W" ||
		len(conf.EquipmentRefs()) != 1 || conf.SubscriptionID == "" {
		t.Errorf("confirmation %+v", conf)
	}
	if conf.PlannedDeparture != "2026-10-07T18:00:00Z" || conf.PlannedArrival != "2026-10-13T08:00:00Z" {
		t.Errorf("schedule %s → %s", conf.PlannedDeparture, conf.PlannedArrival)
	}
	opBooking, err := d.bookings.Get(operatorDomain, conf.BookingRef)
	if err != nil || opBooking.Shipper != shipperDomain {
		t.Errorf("operator's booking: %+v, %v", opBooking, err)
	}
	if _, err := d.ship.Bookings.Get(operatorDomain, conf.BookingRef); err != nil {
		t.Errorf("shipper didn't record the confirmed booking: %v", err)
	}
	if e := d.operator.lastIn(t); e.Outcome != "accepted" || !e.Verified || e.Sender != shipperDomain || e.Intent != intents.BookingRequest {
		t.Errorf("operator's event: %+v", e)
	}
	if e := d.shipper.lastIn(t); e.Outcome != "response" || !e.Verified || e.Intent != intents.BookingConfirmation {
		t.Errorf("shipper's event: %+v", e)
	}
}

// post sends raw bytes to the operator's /ulp, as anyone on the network
// could.
func post(t *testing.T, d *demo, body []byte, contentType string) (int, []byte) {
	t.Helper()
	c := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: d.nw.ca.pool},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, d.operator.addr)
		},
	}}
	resp, err := c.Post("https://"+operatorDomain+"/ulp", contentType, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// seal signs body under h as c, with kid.
func seal(t *testing.T, signer sig.Signer, kid string, h envelope.Header, body any) []byte {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	out, err := envelope.Seal(&envelope.Envelope{Header: h, Body: b}, kid, signer)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// errorCode verifies the operator's signed response and returns the
// ulp.error code it carries.
func errorCode(t *testing.T, d *demo, resp []byte, inReplyTo string) string {
	t.Helper()
	u, err := envelope.Parse(resp)
	if err != nil {
		t.Fatalf("operator's response doesn't parse: %v", err)
	}
	_, v, err := sig.ParseJWK(d.operator.jwk)
	if err != nil {
		t.Fatal(err)
	}
	env, err := u.Verify(v)
	if err != nil {
		t.Fatalf("operator's response doesn't verify: %v", err)
	}
	if inReplyTo != "" && (env.Header.InReplyTo == nil || *env.Header.InReplyTo != inReplyTo) {
		t.Errorf("response inReplyTo %v, want %s", env.Header.InReplyTo, inReplyTo)
	}
	b, err := intents.Decode(env)
	if err != nil {
		t.Fatalf("operator's response body: %v", err)
	}
	e, ok := b.(*intents.ErrorBody)
	if !ok {
		t.Fatalf("response is %s, want ulp.error", env.Header.Intent)
	}
	return e.Code
}

func header(intent, sender, receiver string) envelope.Header {
	return envelope.NewHeader(intent, sender, receiver, time.Now())
}

// TestPipelineRejects sends the operator one bad envelope per pipeline
// step and checks the signed ulp.error code, the logged outcome, and that
// nothing was booked.
func TestPipelineRejects(t *testing.T) {
	d := newDemo(t)
	cy, ob := d.shipper, d.operator
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	otherSigner, _ := sig.NewEd25519Signer(otherPriv)

	type tc struct {
		name     string
		msg      func() ([]byte, string) // the message and its messageId
		code     string
		verified bool
		// status is the bare HTTP refusal for checks before verification;
		// zero means a signed ulp.error (200).
		status int
	}
	request := func(mutate func(h *envelope.Header, b *intents.BookingRequestBody)) func() ([]byte, string) {
		return func() ([]byte, string) {
			h := header(intents.BookingRequest, shipperDomain, operatorDomain)
			b := bookingRequest()
			if mutate != nil {
				mutate(&h, b)
			}
			return seal(t, cy.signer, cy.kid, h, b), h.MessageID
		}
	}
	cases := []tc{
		{"wrong receiver", request(func(h *envelope.Header, _ *intents.BookingRequestBody) { h.Receiver = "other-carrier.example" }),
			intents.CodeForbidden, false, http.StatusMisdirectedRequest},
		// Signed with the operator's own key: it would verify, so it must
		// be refused before that, on the claim alone.
		{"from the node itself", func() ([]byte, string) {
			h := header(intents.BookingRequest, operatorDomain, operatorDomain)
			b := bookingRequest()
			b.Subscription.Endpoint = "https://" + operatorDomain + "/ulp"
			return seal(t, ob.signer, ob.kid, h, b), h.MessageID
		}, intents.CodeForbidden, false, http.StatusForbidden},
		{"intent the operator doesn't serve", func() ([]byte, string) {
			h := header(intents.TnTEvent, shipperDomain, operatorDomain)
			ref := "OB-BKG-1"
			h.BookingRef = &ref
			return seal(t, cy.signer, cy.kid, h, map[string]any{"eventId": "EV-1"}), h.MessageID
		}, intents.CodeUnsupportedIntent, false, http.StatusBadRequest},
		{"unknown sender domain", func() ([]byte, string) {
			h := header(intents.BookingRequest, "mallory.example", operatorDomain)
			b := bookingRequest()
			b.Subscription.Endpoint = "https://mallory.example/ulp"
			return seal(t, d.mallory.signer, d.mallory.kid, h, b), h.MessageID
		}, intents.CodeUnknownKey, false, http.StatusServiceUnavailable}, // its profile can't be fetched
		{"unknown kid", func() ([]byte, string) {
			h := header(intents.BookingRequest, shipperDomain, operatorDomain)
			return seal(t, cy.signer, "cy-2099-1", h, bookingRequest()), h.MessageID
		}, intents.CodeUnknownKey, false, http.StatusForbidden},
		{"signed by another key under the sender's kid", func() ([]byte, string) {
			h := header(intents.BookingRequest, shipperDomain, operatorDomain)
			return seal(t, otherSigner, cy.kid, h, bookingRequest()), h.MessageID
		}, intents.CodeBadSignature, false, http.StatusForbidden},
		{"impersonation: Mallory claims to be Cymbal", func() ([]byte, string) {
			h := header(intents.BookingRequest, shipperDomain, operatorDomain)
			return seal(t, d.mallory.signer, cy.kid, h, bookingRequest()), h.MessageID
		}, intents.CodeBadSignature, false, http.StatusForbidden},
		{"tampered body", func() ([]byte, string) {
			msg, id := request(nil)()
			var w map[string]string
			_ = json.Unmarshal(msg, &w)
			p, _ := base64.RawURLEncoding.DecodeString(w["payload"])
			w["payload"] = base64.RawURLEncoding.EncodeToString(bytes.Replace(p, []byte(`"count":1`), []byte(`"count":9`), 1))
			out, _ := json.Marshal(w)
			return out, id
		}, intents.CodeBadSignature, false, http.StatusForbidden},
		{"stale sentAt", request(func(h *envelope.Header, _ *intents.BookingRequestBody) {
			h.SentAt = time.Now().Add(-10 * time.Minute).UTC().Format(envelope.TimeLayout)
		}), intents.CodeStale, true, 0},
		{"sentAt in the future", request(func(h *envelope.Header, _ *intents.BookingRequestBody) {
			h.SentAt = time.Now().Add(10 * time.Minute).UTC().Format(envelope.TimeLayout)
		}), intents.CodeStale, true, 0},
		{"invalid body", request(func(_ *envelope.Header, b *intents.BookingRequestBody) { b.PortOfLoading = "nowhere" }),
			intents.CodeInvalidBody, true, 0},
		{"header rule: request with bookingRef", request(func(h *envelope.Header, _ *intents.BookingRequestBody) {
			ref := "OB-BKG-1"
			h.BookingRef = &ref
		}), intents.CodeInvalidBody, true, 0},
		{"subscription to a third party", request(func(_ *envelope.Header, b *intents.BookingRequestBody) {
			b.Subscription.Endpoint = "https://victim.example/ulp"
		}), intents.CodeForbidden, true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := d.bookings.Len()
			msg, id := c.msg()
			status, resp := post(t, d, msg, "application/json")
			if c.status != 0 {
				// Unverified: a bare status, and nothing signed.
				if status != c.status || len(resp) != 0 {
					t.Fatalf("HTTP %d with %d bytes, want a bare %d", status, len(resp), c.status)
				}
			} else {
				if status != http.StatusOK {
					t.Fatalf("HTTP %d, want 200 with a signed ulp.error", status)
				}
				if got := errorCode(t, d, resp, id); got != c.code {
					t.Errorf("ulp.error code %q, want %q", got, c.code)
				}
			}
			e := ob.lastIn(t)
			if e.Outcome != "rejected" || e.Code != c.code || e.Verified != c.verified {
				t.Errorf("operator logged %+v; want rejected/%s/verified=%v", e, c.code, c.verified)
			}
			if d.bookings.Len() != before {
				t.Error("a rejected request was booked")
			}
		})
	}
}

func TestReplayRejected(t *testing.T) {
	d := newDemo(t)
	h := header(intents.BookingRequest, shipperDomain, operatorDomain)
	msg := seal(t, d.shipper.signer, d.shipper.kid, h, bookingRequest())
	if status, resp := post(t, d, msg, "application/json"); status != http.StatusOK || !strings.Contains(string(resp), "payload") {
		t.Fatalf("first delivery: HTTP %d", status)
	}
	if d.bookings.Len() != 1 {
		t.Fatalf("first delivery booked %d", d.bookings.Len())
	}
	_, resp := post(t, d, msg, "application/json")
	if got := errorCode(t, d, resp, h.MessageID); got != intents.CodeReplay {
		t.Errorf("second delivery: %q, want replay", got)
	}
	if d.bookings.Len() != 1 {
		t.Error("the replay was booked")
	}
}

// rejecting answers every booking with status "rejected".
type rejecting struct{}

func (rejecting) Authorize(context.Context, *node.Request) error { return nil }

func (rejecting) Handle(context.Context, *node.Request) (*node.Reply, error) {
	return &node.Reply{Body: &intents.BookingConfirmationBody{BookingRef: "OB-BKG-0", Status: intents.StatusRejected,
		Messages: []intents.Message{{Code: "vessel_full", Detail: "No space on 42W."}}}, BookingRef: "OB-BKG-0"}, nil
}

// TestRejectedBookingNotRecorded: the shipper records only confirmed
// bookings; a "rejected" answer goes back to the caller.
func TestRejectedBookingNotRecorded(t *testing.T) {
	d := newDemo(t)
	d.nw.wire(t, d.operator, "Ocean Blue", "operator", map[string]node.Handler{"ulp.ocean.booking.request": rejecting{}})
	conf, err := d.ship.Book(context.Background(), operatorDomain, bookingRequest())
	if err != nil || conf.Status != intents.StatusRejected {
		t.Fatalf("Book = %+v, %v", conf, err)
	}
	if d.ship.Bookings.Len() != 0 {
		t.Error("a rejected booking was recorded")
	}
}

func TestDenyList(t *testing.T) {
	d := newDemo(t)
	d.booking.Deny[shipperDomain] = true
	_, err := d.ship.Book(context.Background(), operatorDomain, bookingRequest())
	var re *node.RemoteError
	if !errors.As(err, &re) || re.Code != intents.CodeForbidden {
		t.Errorf("Book by a denied shipper = %v, want a forbidden RemoteError", err)
	}
	if d.bookings.Len() != 0 {
		t.Error("a denied request was booked")
	}
}

func TestTransportErrors(t *testing.T) {
	d := newDemo(t)
	for name, body := range map[string][]byte{
		"not JSON":        []byte("book me a ship"),
		"compact JWS":     []byte(`"a.b.c"`),
		"unsigned header": []byte(`{"protected":"e30","payload":"e30","signature":"AA"}`),
	} {
		if status, resp := post(t, d, body, "application/json"); status != http.StatusBadRequest || len(resp) != 0 {
			t.Errorf("%s: HTTP %d with %d bytes, want 400 with none", name, status, len(resp))
		}
	}
	if e := d.operator.lastIn(t); e.Outcome != "malformed" {
		t.Errorf("logged %+v, want malformed", e)
	}
	if status, _ := post(t, d, []byte("{}"), "text/plain"); status != http.StatusUnsupportedMediaType {
		t.Errorf("wrong content type: HTTP %d", status)
	}
	if status, _ := post(t, d, bytes.Repeat([]byte("x"), envelope.MaxSize+1), "application/json"); status != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize: HTTP %d", status)
	}
}
