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
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
)

func confirmationFor(ref string) *intents.BookingConfirmationBody {
	return &intents.BookingConfirmationBody{
		BookingRef: ref, Status: intents.StatusConfirmed,
		EBL:              &intents.EBL{TransportDocumentReference: "OB-TD-1", VesselName: "Ocean Blue Aurora", VoyageNumber: "42W", PortOfLoading: "NLRTM", PortOfDischarge: "USEWR", ShipperName: "Cymbal Shoes", ConsigneeName: "To order", Status: "BOOKED", Containers: []intents.ShippingContainer{{ContainerNumber: "OCBU0000004", EquipmentSizeType: "45G1", SealNumber: "S1", GrossWeightKg: 12000}}},
		PlannedDeparture: "2026-10-07T18:00:00Z", PlannedArrival: "2026-10-13T08:00:00Z",
	}
}

// TestForgedResponsesRejected puts a rogue server at the operator's
// address. It holds the operator's real key (worst case) but answers
// wrongly in one way each time; the shipper must refuse every answer and
// record nothing.
func TestForgedResponsesRejected(t *testing.T) {
	d := newDemo(t)
	ob := d.operator
	type forge func(req envelope.Header) []byte
	reply := func(mutate func(req envelope.Header, h *envelope.Header) any) forge {
		return func(req envelope.Header) []byte {
			h := req.Reply(intents.BookingConfirmation, time.Now())
			ref := "OB-BKG-7"
			h.BookingRef = &ref
			var body any = confirmationFor(ref)
			if mutate != nil {
				if b := mutate(req, &h); b != nil {
					body = b
				}
			}
			return seal(t, ob.signer, ob.kid, h, body)
		}
	}
	cases := map[string]forge{
		"answers another message": reply(func(_ envelope.Header, h *envelope.Header) any {
			other := envelope.NewHeader(intents.BookingRequest, shipperDomain, operatorDomain, time.Now()).MessageID
			h.InReplyTo = &other
			return nil
		}),
		"another conversation": reply(func(_ envelope.Header, h *envelope.Header) any {
			h.CorrelationID = envelope.NewHeader(intents.BookingRequest, shipperDomain, operatorDomain, time.Now()).MessageID
			return nil
		}),
		"addressed to someone else": reply(func(_ envelope.Header, h *envelope.Header) any {
			h.Receiver = "cymbal-footwear.example"
			return nil
		}),
		"claims to be from another party": reply(func(_ envelope.Header, h *envelope.Header) any {
			h.Sender = "mallory.example"
			return nil
		}),
		"stale": reply(func(_ envelope.Header, h *envelope.Header) any {
			h.SentAt = time.Now().Add(-time.Hour).UTC().Format(envelope.TimeLayout)
			return nil
		}),
		"unexpected intent": func(req envelope.Header) []byte {
			h := req.Reply(intents.BookingRequest, time.Now())
			return seal(t, ob.signer, ob.kid, h, bookingRequest())
		},
		"invalid body": reply(func(_ envelope.Header, _ *envelope.Header) any {
			c := confirmationFor("OB-BKG-7")
			c.Status = "maybe"
			return c
		}),
		"signed by the shipper's own key": func(req envelope.Header) []byte {
			h := req.Reply(intents.BookingConfirmation, time.Now())
			ref := "OB-BKG-7"
			h.BookingRef = &ref
			return seal(t, d.shipper.signer, ob.kid, h, confirmationFor(ref))
		},
		"tampered after signing": func(req envelope.Header) []byte {
			msg := reply(nil)(req)
			var w map[string]string
			_ = json.Unmarshal(msg, &w)
			p, _ := base64.RawURLEncoding.DecodeString(w["payload"])
			w["payload"] = base64.RawURLEncoding.EncodeToString(bytes.Replace(p, []byte("42W"), []byte("43W"), 1))
			out, _ := json.Marshal(w)
			return out
		},
		"not an envelope": func(envelope.Header) []byte { return []byte(`{"status":"confirmed"}`) },
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			ob.mu.Lock()
			real := ob.handler
			ob.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet { // profile and keys stay genuine
					real.ServeHTTP(w, r)
					return
				}
				body, _ := io.ReadAll(r.Body)
				u, err := envelope.Parse(body)
				if err != nil {
					t.Errorf("rogue server got an unparseable request: %v", err)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(f(u.ClaimedHeader()))
			})
			ob.mu.Unlock()
			defer func() { ob.mu.Lock(); ob.handler = real; ob.mu.Unlock() }()

			conf, err := d.ship.Book(context.Background(), operatorDomain, bookingRequest())
			if !errors.Is(err, node.ErrBadResponse) {
				t.Errorf("Book = %+v, %v; want ErrBadResponse", conf, err)
			}
			if d.ship.Bookings.Len() != 0 {
				t.Error("the shipper recorded a booking from a forged response")
			}
			if e := d.shipper.lastIn(t); e.Outcome != "rejected" {
				t.Errorf("shipper logged %+v, want rejected", e)
			}
		})
	}
}

// TestExchangeTransportFailures: an operator that is down, answers 204 to a
// request that needs an answer, or answers HTTP errors.
func TestExchangeTransportFailures(t *testing.T) {
	d := newDemo(t)
	ob := d.operator
	for name, h := range map[string]http.HandlerFunc{
		"204": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) },
		"500": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) },
		"HTML": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<h1>hi</h1>"))
		},
		"redirects": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://mallory.example/ulp", http.StatusTemporaryRedirect)
		},
	} {
		t.Run(name, func(t *testing.T) {
			ob.mu.Lock()
			real := ob.handler
			ob.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					real.ServeHTTP(w, r)
					return
				}
				h(w, r)
			})
			ob.mu.Unlock()
			defer func() { ob.mu.Lock(); ob.handler = real; ob.mu.Unlock() }()
			if _, err := d.ship.Book(context.Background(), operatorDomain, bookingRequest()); err == nil {
				t.Error("Book succeeded")
			}
			if d.ship.Bookings.Len() != 0 {
				t.Error("recorded a booking")
			}
		})
	}
	// An operator nobody can resolve.
	if _, err := d.ship.Book(context.Background(), "nowhere.example", bookingRequest()); err == nil {
		t.Error("Book to an unresolvable operator succeeded")
	}
}
