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

func (p poster) PostTo(ctx context.Context, r, _ string, m []byte) ([]byte, error) {
	return p(ctx, r, m)
}

// withRef is an answer whose header names ref, not the booking asked.
type withRef struct {
	ref  string
	body any
}

func shipperWith(t *testing.T, answer func(req envelope.Header) (string, any), opts ...func(*node.Config)) *Shipper {
	t.Helper()
	cyPub, cyPriv, _ := ed25519.GenerateKey(rand.Reader)
	obPub, obPriv, _ := ed25519.GenerateKey(rand.Reader)
	cySigner, _ := sig.NewEd25519Signer(cyPriv)
	obSigner, _ := sig.NewEd25519Signer(obPriv)
	cyJWK, _ := sig.Ed25519JWK(cyPub, "cy-1")
	obV, _ := sig.NewEd25519Verifier(obPub)
	cfg := node.Config{
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
			if intent == "" {
				return nil, rest.ErrNoResponse // a push: accepted, no answer
			}
			h := req.Reply(intent, time.Now())
			intents.SetDialect(&h)
			if c, ok := body.(*intents.BookingConfirmationBody); ok {
				h.BookingRef = &c.BookingRef
			}
			if c, ok := body.(*intents.RoadBookingConfirmationBody); ok {
				h.BookingRef = &c.BookingRef
			}
			if c, ok := body.(*intents.ForwardingConfirmationBody); ok {
				h.BookingRef = &c.OrderRef
			}
			if c, ok := body.(*intents.BookingCancellationResponseBody); ok {
				h.BookingRef = &c.BookingRef
			}
			if c, ok := body.(*intents.ForwardingStatusResponseBody); ok && c.OrderRef != "" {
				h.BookingRef = &c.OrderRef
			}
			if _, ok := body.(*intents.TnTSubscriptionConfirmationBody); ok {
				h.BookingRef = req.BookingRef
			}
			if w, ok := body.(withRef); ok {
				// The answer names another booking than asked.
				body, h.BookingRef = w.body, &w.ref
			}
			b, _ := json.Marshal(body)
			return envelope.Seal(&envelope.Envelope{Header: h, Body: b}, "ob-1", obSigner)
		}),
	}
	for _, o := range opts {
		o(&cfg)
	}
	n, err := node.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &Shipper{Node: n, Bookings: store.NewBookings(), Now: time.Now}
}

func request() *intents.BookingRequestBody {
	return &intents.BookingRequestBody{
		ShipperRef: "P1", EarliestDeparture: "2026-10-06",
		ShipperName:     "Cymbal Shoes",
		PortOfLoading:   "NLRTM",
		PortOfDischarge: "USEWR",
		Equipment:       []intents.Equipment{{ISOType: "45G1", Count: 1, GrossWeightKg: 12000}},
		Cargo:           intents.Cargo{Description: "Shoes", HSCode: "6404"},
	}
}

func TestBook(t *testing.T) {
	s := shipperWith(t, func(envelope.Header) (string, any) {
		return intents.BookingConfirmation, &intents.BookingConfirmationBody{
			BookingRef: "OB-BKG-1", Status: intents.StatusConfirmed,
			EBL:              &intents.EBL{TransportDocumentReference: "OB-TD-1", VesselName: "Aurora", VoyageNumber: "42W", PortOfLoading: "NLRTM", PortOfDischarge: "USEWR", ShipperName: "Cymbal Shoes", ConsigneeName: "To order", Status: "BOOKED", Containers: []intents.ShippingContainer{{ContainerNumber: "OCBU0000004", EquipmentSizeType: "45G1", SealNumber: "S1", GrossWeightKg: 12000}}},
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

// TestBookRefusesContradictingEBL: a signed confirmation whose eBL
// doesn't describe what was asked for is an error, and nothing is recorded.
func TestBookRefusesContradictingEBL(t *testing.T) {
	good := func() *intents.EBL {
		return &intents.EBL{TransportDocumentReference: "OB-TD-1", VesselName: "Aurora", VoyageNumber: "42W", PortOfLoading: "NLRTM",
			PortOfDischarge: "USEWR", ShipperName: "Cymbal Shoes", ConsigneeName: "Cymbal Footwear", Status: "BOOKED",
			Containers: []intents.ShippingContainer{{ContainerNumber: "OCBU0000004", EquipmentSizeType: "45G1", SealNumber: "S1", GrossWeightKg: 12000}}}
	}
	other := intents.ShippingContainer{ContainerNumber: "OCBU0000015", EquipmentSizeType: "45G1", SealNumber: "S2", GrossWeightKg: 12000}
	for name, edit := range map[string]func(*intents.EBL){
		"another port of loading":   func(e *intents.EBL) { e.PortOfLoading = "DEHAM" },
		"another port of discharge": func(e *intents.EBL) { e.PortOfDischarge = "USLAX" },
		"another shipper":           func(e *intents.EBL) { e.ShipperName = "Mallory Freight" },
		"another consignee":         func(e *intents.EBL) { e.ConsigneeName = "Mallory" },
		"a service contract":        func(e *intents.EBL) { e.ServiceContractNumber = "SC-1" },
		"not BOOKED":                func(e *intents.EBL) { e.Status = "LOADED" },
		"an extra container":        func(e *intents.EBL) { e.Containers = append(e.Containers, other) },
		"another size-type":         func(e *intents.EBL) { e.Containers[0].EquipmentSizeType = "22G1" },
		"another weight":            func(e *intents.EBL) { e.Containers[0].GrossWeightKg = 30000 },
	} {
		t.Run(name, func(t *testing.T) {
			e := good()
			edit(e)
			s := shipperWith(t, func(envelope.Header) (string, any) {
				return intents.BookingConfirmation, &intents.BookingConfirmationBody{
					BookingRef: "OB-BKG-1", Status: intents.StatusConfirmed, EBL: e,
					PlannedDeparture: "2026-10-07T18:00:00Z", PlannedArrival: "2026-10-13T08:00:00Z",
				}
			})
			req := request()
			req.Consignee = &intents.Consignee{Name: "Cymbal Footwear", Domain: "cymbal-footwear.example"}
			if conf, err := s.Book(context.Background(), "ocean-blue.example", req); !errors.Is(err, ErrEBLMismatch) {
				t.Errorf("Book = %+v, %v; want ErrEBLMismatch", conf, err)
			}
			if s.Bookings.Len() != 0 {
				t.Error("a contradicting eBL was recorded")
			}
		})
	}
	if err := checkEBL(request(), nil); err == nil {
		t.Error("no eBL passed")
	}
	// Without a named consignee, the operator's choice ("To order") stands.
	if err := checkEBL(request(), good()); err != nil {
		t.Errorf("the matching eBL: %v", err)
	}
}

// TestBookRefusesReusedReference: an operator confirming a reference it
// already used doesn't overwrite the booking.
func TestBookRefusesReusedReference(t *testing.T) {
	s := shipperWith(t, func(envelope.Header) (string, any) {
		return intents.BookingConfirmation, &intents.BookingConfirmationBody{
			BookingRef: "OB-BKG-1", Status: intents.StatusConfirmed,
			EBL:              &intents.EBL{TransportDocumentReference: "OB-TD-1", VesselName: "Aurora", VoyageNumber: "42W", PortOfLoading: "NLRTM", PortOfDischarge: "USEWR", ShipperName: "Cymbal Shoes", ConsigneeName: "To order", Status: "BOOKED", Containers: []intents.ShippingContainer{{ContainerNumber: "OCBU0000004", EquipmentSizeType: "45G1", SealNumber: "S1", GrossWeightKg: 12000}}},
			PlannedDeparture: "2026-10-07T18:00:00Z", PlannedArrival: "2026-10-13T08:00:00Z",
		}
	})
	if _, err := s.Book(context.Background(), "ocean-blue.example", request()); err != nil {
		t.Fatal(err)
	}
	second := request()
	second.ShipperRef = "P2"
	if _, err := s.Book(context.Background(), "ocean-blue.example", second); !errors.Is(err, store.ErrExists) || !strings.Contains(err.Error(), "again") {
		t.Errorf("a reused reference: %v, want ErrExists", err)
	}
	if b, _ := s.Bookings.Get("ocean-blue.example", "OB-BKG-1"); b.Request.ShipperRef != "P1" {
		t.Errorf("the first booking was overwritten: %+v", b.Request)
	}
}

// TestBookStoreFull: a confirmation the shipper can't record is an error,
// and not one that blames the operator for reusing a reference.
func TestBookStoreFull(t *testing.T) {
	s := shipperWith(t, func(envelope.Header) (string, any) {
		return intents.BookingConfirmation, &intents.BookingConfirmationBody{
			BookingRef: "OB-BKG-1", Status: intents.StatusConfirmed,
			EBL:              &intents.EBL{TransportDocumentReference: "OB-TD-1", VesselName: "Aurora", VoyageNumber: "42W", PortOfLoading: "NLRTM", PortOfDischarge: "USEWR", ShipperName: "Cymbal Shoes", ConsigneeName: "To order", Status: "BOOKED", Containers: []intents.ShippingContainer{{ContainerNumber: "OCBU0000004", EquipmentSizeType: "45G1", SealNumber: "S1", GrossWeightKg: 12000}}},
			PlannedDeparture: "2026-10-07T18:00:00Z", PlannedArrival: "2026-10-13T08:00:00Z",
		}
	})
	s.Bookings = store.NewBookingsCap(0)
	if _, err := s.Book(context.Background(), "ocean-blue.example", request()); !errors.Is(err, store.ErrFull) || strings.Contains(err.Error(), "again") {
		t.Errorf("Book into a full store: %v, want ErrFull", err)
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
