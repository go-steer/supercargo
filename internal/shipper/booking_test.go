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
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/discovery"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
	"github.com/go-steer/supercargo/pkg/sig"
)

type keys map[string]sig.Verifier

func (k keys) Key(_ context.Context, domain, kid string) (sig.Verifier, error) {
	if v, ok := k[domain+"/"+kid]; ok {
		return v, nil
	}
	return nil, discovery.ErrUnknownKey
}

// operatorAnswering returns a Poster that answers every request as the
// operator would, with body.
type poster func(context.Context, string, []byte) ([]byte, error)

func (p poster) Post(ctx context.Context, r string, m []byte) ([]byte, error) { return p(ctx, r, m) }

func shipperWith(t *testing.T, answer func(req envelope.Header) (string, any)) *Shipper {
	t.Helper()
	cyPub, cyPriv, _ := ed25519.GenerateKey(rand.Reader)
	obPub, obPriv, _ := ed25519.GenerateKey(rand.Reader)
	cySigner, _ := sig.NewEd25519Signer(cyPriv)
	obSigner, _ := sig.NewEd25519Signer(obPriv)
	cyJWK, _ := sig.Ed25519JWK(cyPub, "cy-1")
	obV, _ := sig.NewEd25519Verifier(obPub)
	n, err := node.New(node.Config{
		Domain: "cymbal-shoes.example", Name: "Cymbal Shoes", Roles: []string{"shipper"},
		KID: "cy-1", Signer: cySigner, PublicJWK: cyJWK,
		Keys: keys{"ocean-blue.example/ob-1": obV}, Replay: store.NewReplay(store.ReplayOptions{}),
		Poster: poster(func(_ context.Context, _ string, msg []byte) ([]byte, error) {
			u, err := envelope.Parse(msg)
			if err != nil {
				return nil, err
			}
			req := u.ClaimedHeader()
			intent, body := answer(req)
			h := req.Reply(intent, time.Now())
			if c, ok := body.(*intents.BookingConfirmationBody); ok {
				h.BookingRef = &c.BookingRef
			}
			b, _ := json.Marshal(body)
			return envelope.Seal(&envelope.Envelope{Header: h, Body: b}, "ob-1", obSigner)
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &Shipper{Node: n, Bookings: store.NewBookings(), Now: time.Now}
}

func request() *intents.BookingRequestBody {
	return &intents.BookingRequestBody{
		ShipperRef: "P1", EarliestDeparture: "2026-10-06",
		Origin:      intents.Location{UNLocode: "NLRTM", Name: "Rotterdam"},
		Destination: intents.Location{UNLocode: "USEWR", Name: "Newark"},
		Equipment:   []intents.Equipment{{ISOType: "45G1", Label: "40HC", Count: 1}},
		Cargo:       intents.Cargo{Description: "Shoes", HSCode: "6404"},
	}
}

func TestBook(t *testing.T) {
	s := shipperWith(t, func(envelope.Header) (string, any) {
		return intents.BookingConfirmation, &intents.BookingConfirmationBody{
			BookingRef: "OB-BKG-1", Status: intents.StatusConfirmed,
			Vessel:           &intents.Vessel{Name: "Aurora", Voyage: "42W"},
			PlannedDeparture: "2026-10-07T18:00:00Z", PlannedArrival: "2026-10-13T08:00:00Z",
		}
	})
	conf, err := s.Book(context.Background(), "ocean-blue.example", request())
	if err != nil || conf.BookingRef != "OB-BKG-1" {
		t.Fatalf("Book = %+v, %v", conf, err)
	}
	if b, err := s.Bookings.Get("ocean-blue.example", "OB-BKG-1"); err != nil || b.Operator != "ocean-blue.example" {
		t.Errorf("recorded %+v, %v", b, err)
	}
}

// TestBookRefusesReusedReference: an operator confirming a reference it
// already used doesn't overwrite the booking.
func TestBookRefusesReusedReference(t *testing.T) {
	s := shipperWith(t, func(envelope.Header) (string, any) {
		return intents.BookingConfirmation, &intents.BookingConfirmationBody{
			BookingRef: "OB-BKG-1", Status: intents.StatusConfirmed,
			Vessel:           &intents.Vessel{Name: "Aurora", Voyage: "42W"},
			PlannedDeparture: "2026-10-07T18:00:00Z", PlannedArrival: "2026-10-13T08:00:00Z",
		}
	})
	if _, err := s.Book(context.Background(), "ocean-blue.example", request()); err != nil {
		t.Fatal(err)
	}
	second := request()
	second.ShipperRef = "P2"
	if _, err := s.Book(context.Background(), "ocean-blue.example", second); err == nil {
		t.Error("a reused reference was accepted")
	}
	if b, _ := s.Bookings.Get("ocean-blue.example", "OB-BKG-1"); b.Request.ShipperRef != "P1" {
		t.Errorf("the first booking was overwritten: %+v", b.Request)
	}
}

func TestBookNotRecorded(t *testing.T) {
	for _, status := range []string{intents.StatusRejected, intents.StatusAlternativeProposed} {
		s := shipperWith(t, func(envelope.Header) (string, any) {
			return intents.BookingConfirmation, &intents.BookingConfirmationBody{BookingRef: "OB-BKG-1", Status: status}
		})
		conf, err := s.Book(context.Background(), "ocean-blue.example", request())
		if err != nil || conf.Status != status {
			t.Fatalf("%s: Book = %+v, %v", status, conf, err)
		}
		if s.Bookings.Len() != 0 {
			t.Errorf("%s: recorded a booking", status)
		}
	}
	s := shipperWith(t, func(envelope.Header) (string, any) {
		return intents.Error, &intents.ErrorBody{Code: intents.CodeForbidden, Detail: "No."}
	})
	_, err := s.Book(context.Background(), "ocean-blue.example", request())
	var re *node.RemoteError
	if !errors.As(err, &re) || re.Code != intents.CodeForbidden || s.Bookings.Len() != 0 {
		t.Errorf("Book answered by ulp.error = %v", err)
	}
}
