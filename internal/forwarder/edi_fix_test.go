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
	"errors"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/config"
	"github.com/go-steer/supercargo/internal/edi"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/intents"
)

var tilburgToSantos = config.Lane{From: "NLTLB", To: "BRSSZ", Legs: []config.LaneLeg{
	{Mode: "road", Carrier: polder, Name: "Polder Haulage", To: "NLRTM", Facility: "Euromax terminal", EDI: true},
	{Mode: "ocean", Carrier: "ocean-blue.example", Name: "Ocean Blue", To: "BRSSZ"},
}}

func tilburgOrder() *intents.ForwardingRequestBody {
	o := anOrder()
	o.PlaceOfReceipt = intents.Place{Type: intents.PlaceDoor, UNLocationCode: "NLTLB",
		Address: &intents.Address{Street: "Ringbaan Oost 100", Locality: "Tilburg", Country: "NL"}}
	return o
}

// TestBookEDIRetry: a booking retried after the acknowledgement was late
// sends the same instruction (its pickup time as first sent) and gets the
// answer; nothing is refused as "instructed differently".
func TestBookEDIRetry(t *testing.T) {
	r := newEDIRig(t)
	r.gw.AckTimeout = 100 * time.Millisecond
	orch, _ := orchestrated(t, &carriers{}, tilburgToSantos)
	orch.EDI = r.gw
	if _, err := orch.Orders.Handle(context.Background(), orderFrom(cymbal, tilburgOrder())); err != nil {
		t.Fatal(err)
	}
	if _, err := orch.Book(context.Background(), "MF-0007"); !errors.Is(err, ErrNoAck) {
		t.Fatalf("no answer yet: %v", err)
	}
	// An hour later, Polder answers; the person books again.
	orch.Now = func() time.Time { return time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC) }
	r.run(t)
	r.gw.AckTimeout = 5 * time.Second
	if _, err := orch.Book(context.Background(), "MF-0007"); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	b, _ := orch.Orders.Bookings.Get(meridian, "MF-0007")
	if l := b.Order.Legs[0]; l.Ref != "MF-0007-L1" || l.Departure != "2026-10-05T09:00:00Z" {
		t.Errorf("the EDI leg %+v", l)
	}
}

// TestBookEDIRefusedThenRetried: an instruction the carrier refused is
// sent anew under the next attempt's reference.
func TestBookEDIRefusedThenRetried(t *testing.T) {
	r := newEDIRig(t)
	orch, _ := orchestrated(t, &carriers{}, tilburgToSantos)
	orch.EDI = r.gw
	if _, err := orch.Orders.Handle(context.Background(), orderFrom(cymbal, tilburgOrder())); err != nil {
		t.Fatal(err)
	}
	// Polder refuses the first instruction (written by hand: the mock
	// takes everything).
	go func() {
		for range 50 {
			if _, _, ok := r.gw.Instructed("MF-0007-L1"); ok {
				data, _ := edi.WriteAPERAK(edi.Ack{Reference: "MF-0007-L1", CarrierRef: "PH1", Errors: []string{"12"}},
					edi.Envelope{Sender: polder, Recipient: meridian, Control: "PH1", At: time.Now()})
				_, _ = r.gw.Partners[polder].In.Put("PH1", data)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	if _, err := orch.Book(context.Background(), "MF-0007"); !errors.Is(err, ErrLegRefused) {
		t.Fatalf("refused: %v", err)
	}
	r.run(t)
	if _, err := orch.Book(context.Background(), "MF-0007"); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	b, _ := orch.Orders.Bookings.Get(meridian, "MF-0007")
	if l := b.Order.Legs[0]; l.Ref != "MF-0007-L1-2" {
		t.Errorf("the EDI leg %+v", l)
	}
}

// TestUnsignedBounds: a status re-issued once, however often it's sent; a
// status dated far off isn't; a container not among the order's (signed)
// legs' isn't named.
func TestUnsignedBounds(t *testing.T) {
	r, pub, _ := relayed(t)
	_, _ = r.Orders.Bookings.Update(meridian, "MF-0007", func(b *store.Booking) error {
		b.Order.Legs = append([]store.LegRef{{Mode: intents.ModeRoad, Carrier: polder, Name: "Polder Haulage", Ref: "MF-0007-L1",
			From: "NLTLB", To: "NLRTM", Unsigned: true}}, b.Order.Legs...)
		return nil
	})
	now := time.Now().UTC().Truncate(time.Minute)
	s := edi.Status{Reference: "MF-0007-L1", Meaning: edi.PickedUp, Code: "12", Time: now, Sender: polder, Equipment: "MSCU1234565"}
	if err := r.Unsigned(polder, s); err != nil {
		t.Fatal(err)
	}
	if e := pub.bodies[0].(*intents.TnTEventBody); e.EquipmentReference != "" || e.EventType != intents.EventTypeTransport {
		t.Errorf("another customer's container named: %+v", e)
	}
	if err := r.Unsigned(polder, s); !errors.Is(err, ErrSkipped) || len(pub.bodies) != 1 {
		t.Errorf("the same status again: %v", err)
	}
	s.Time, s.Code = now.Add(2*StatusWindow), "13"
	if err := r.Unsigned(polder, s); !errors.Is(err, ErrSkipped) {
		t.Errorf("dated far off: %v", err)
	}
	// The order's own container (from Ocean Blue's signed leg) is named.
	s.Time, s.Code, s.Equipment = now.Add(time.Minute), "12", "OCBU0000004"
	if err := r.Unsigned(polder, s); err != nil {
		t.Fatal(err)
	}
	if e := pub.bodies[1].(*intents.TnTEventBody); e.EquipmentReference != "OCBU0000004" {
		t.Errorf("the order's container: %+v", e)
	}
}
