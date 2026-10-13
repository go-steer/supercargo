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
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/edi"
	"github.com/go-steer/supercargo/internal/mockcarrier"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/intents"
)

const polder = "polder-haulage.example"

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// ediRig is a gateway to Polder, the mock carrier on the other side of
// the same file drops, and the statuses the gateway hands on.
type ediRig struct {
	gw     *Gateway
	polder *mockcarrier.EDIFACT
	mu     sync.Mutex
	got    []edi.Status
}

func newEDIRig(t *testing.T) *ediRig {
	t.Helper()
	outPath, inPath := t.TempDir(), t.TempDir()
	open := func(p string) *edi.Dir {
		d, err := edi.NewDir(p)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = d.Close() })
		return d
	}
	r := &ediRig{}
	r.gw = &Gateway{Self: meridian, Partners: map[string]*EDIPartner{polder: {ID: polder, Out: open(outPath), In: open(inPath), Transit: 3 * time.Hour}},
		Status: func(_ string, s edi.Status) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.got = append(r.got, s)
			return nil
		},
		AckTimeout: 5 * time.Second, PollEvery: 20 * time.Millisecond, Now: time.Now, Log: quiet}
	r.polder = &mockcarrier.EDIFACT{ID: polder, Partner: meridian, In: open(outPath), Out: open(inPath),
		ECMRPrefix: "ECMR-NL-2026", HoursPerSecond: 3600, Now: time.Now, Log: quiet}
	return r
}

func (r *ediRig) run(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.polder.Run(ctx, 20*time.Millisecond) }()
	t.Cleanup(func() { cancel(); <-done })
}

func anInstruction() edi.Instruction {
	return edi.Instruction{Reference: "MF-0007-L1", CargoDescription: "Footwear",
		Pickup:     edi.Stop{Name: "Cymbal warehouse", Country: "NL", UNLocode: "NLTLB", Earliest: time.Now().Add(time.Hour).UTC().Truncate(time.Minute)},
		Delivery:   edi.Stop{Name: "Euromax terminal", Country: "NL", UNLocode: "NLRTM"},
		Containers: []edi.Container{{SizeType: "45G1", Count: 1, GrossWeightKg: 12000}}}
}

// TestInstruct: an instruction is acknowledged; instructing again sends
// nothing new and gets the same answer; a different one under the same
// reference is refused; the carrier's statuses are handed on.
func TestInstruct(t *testing.T) {
	r := newEDIRig(t)
	r.run(t)
	ack, err := r.gw.Instruct(context.Background(), polder, anInstruction())
	if err != nil || !ack.Accepted || ack.TransportDocuments[0] != "ECMR-NL-2026-004471" {
		t.Fatalf("%+v, %v", ack, err)
	}
	again, err := r.gw.Instruct(context.Background(), polder, anInstruction())
	if err != nil || again.CarrierRef != ack.CarrierRef {
		t.Errorf("again: %+v, %v", again, err)
	}
	changed := anInstruction()
	changed.CargoDescription = "Boots"
	if _, err := r.gw.Instruct(context.Background(), polder, changed); err == nil {
		t.Error("a changed instruction under the same reference")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		r.gw.Step()
		r.mu.Lock()
		n := len(r.got)
		r.mu.Unlock()
		if n == 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d statuses handed on", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if r.got[0].Meaning != edi.PickedUp || r.got[0].Reference != "MF-0007-L1" || r.got[0].Sender != polder {
		t.Errorf("first status %+v", r.got[0])
	}
}

// TestInstructRefused: no partner, no answer in time, a cancelled wait.
func TestInstructRefused(t *testing.T) {
	r := newEDIRig(t)
	if _, err := r.gw.Instruct(context.Background(), "other.example", anInstruction()); !errors.Is(err, ErrNoPartner) {
		t.Errorf("no partner: %v", err)
	}
	r.gw.AckTimeout = 50 * time.Millisecond
	if _, err := r.gw.Instruct(context.Background(), polder, anInstruction()); !errors.Is(err, ErrNoAck) {
		t.Errorf("no answer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.gw.AckTimeout = time.Minute
	if _, err := r.gw.Instruct(ctx, polder, anInstruction()); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
	bad := anInstruction()
	bad.Reference = "MF-0007-L9"
	bad.Containers = nil
	if _, err := r.gw.Instruct(context.Background(), polder, bad); err == nil {
		t.Error("an invalid instruction")
	}
}

// TestGatewayRefusesStrangers: an acknowledgement or a status from
// another identity, or for a reference not instructed, isn't taken.
func TestGatewayRefusesStrangers(t *testing.T) {
	r := newEDIRig(t)
	in := r.gw.Partners[polder].In
	put := func(sender, ref string, kind string) {
		env := edi.Envelope{Sender: sender, Recipient: meridian, Control: "X1", At: time.Now()}
		var data []byte
		var err error
		if kind == "ack" {
			data, err = edi.WriteAPERAK(edi.Ack{Reference: ref, CarrierRef: "PH1", Accepted: true}, env)
		} else {
			data, err = edi.WriteIFTSTA([]edi.Status{{Reference: ref, Code: "12", Time: time.Now()}}, env)
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := in.Put("X1", data); err != nil {
			t.Fatal(err)
		}
	}
	r.gw.instructed = map[string]instructed{"MF-0007-L1": {carrier: polder}}
	put("mallory.example", "MF-0007-L1", "ack")
	put(polder, "MF-0009-L1", "ack")
	put(polder, "MF-0007-L1", "status") // not acknowledged yet
	_, _ = in.Put("X2", []byte("UNH+1+IFTMIN:D:04A:UN'UNT+2+1'"))
	r.gw.Step()
	if r.gw.instructed["MF-0007-L1"].ack != nil || len(r.got) != 0 {
		t.Errorf("took a stranger's reply: %+v, %d statuses", r.gw.instructed["MF-0007-L1"], len(r.got))
	}
}

// TestRelayUnsigned: an EDI status becomes the order's event, marked
// derived from an unsigned source, in the forwarder's words; a code the
// mapping doesn't know, or a leg that's no order's, isn't re-issued.
func TestRelayUnsigned(t *testing.T) {
	r, pub, _ := relayed(t)
	_, _ = r.Orders.Bookings.Update(meridian, "MF-0007", func(b *store.Booking) error {
		b.Order.Legs = append([]store.LegRef{{Mode: intents.ModeRoad, Carrier: polder, Name: "Polder Haulage", Ref: "MF-0007-L1",
			From: "NLTLB", To: "NLRTM", Unsigned: true}}, b.Order.Legs...)
		return nil
	})
	at := time.Date(2026, 10, 6, 7, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		s          edi.Status
		code, typ  string
		loc, equip string
	}{
		{edi.Status{Meaning: edi.PickedUp, Code: "12"}, "DEPA", intents.EventTypeTransport, "NLTLB", ""},
		{edi.Status{Meaning: edi.PickedUp, Code: "12", Equipment: "OCBU0000004"}, "PICK", intents.EventTypeEquipment, "NLTLB", "OCBU0000004"},
		{edi.Status{Meaning: edi.Departed, Code: "13"}, "DEPA", intents.EventTypeTransport, "NLTLB", ""},
		{edi.Status{Meaning: edi.Arrived, Code: "14", UNLocode: "NLRTM"}, "ARRI", intents.EventTypeTransport, "NLRTM", ""},
		{edi.Status{Meaning: edi.GateIn, Code: "54", Equipment: "OCBU0000004"}, "GTIN", intents.EventTypeEquipment, "NLRTM", "OCBU0000004"},
		{edi.Status{Meaning: edi.Delivered, Code: "21"}, "ARRI", intents.EventTypeTransport, "NLRTM", ""},
	} {
		s := tc.s
		s.Reference, s.Time, s.Sender, s.CarrierRef = "MF-0007-L1", at, polder, "PH4471"
		n := len(pub.bodies)
		if err := r.Unsigned(polder, s); err != nil {
			t.Fatalf("%s: %v", s.Meaning, err)
		}
		e := pub.bodies[n].(*intents.TnTEventBody)
		if e.EventTypeCode != tc.code || e.EventType != tc.typ || e.UNLocationCode != tc.loc || e.EquipmentReference != tc.equip ||
			len(e.DerivedFrom) != 1 || e.DerivedFrom[0].Signed || e.DerivedFrom[0].Source != "EDIFACT IFTSTA from polder-haulage.example" ||
			e.DerivedFrom[0].Reference != "PH4471" || e.EventDateTime != "2026-10-06T07:00:00Z" {
			t.Errorf("%s: %+v", s.Meaning, e)
		}
	}
	if err := r.Unsigned(polder, edi.Status{Reference: "MF-0007-L1", Meaning: edi.Unknown, Code: "99", Time: at}); err == nil {
		t.Error("an unknown code was re-issued")
	}
	if err := r.Unsigned(polder, edi.Status{Reference: "MF-0009-L1", Meaning: edi.PickedUp, Code: "12", Time: at}); err == nil {
		t.Error("no order's leg")
	}
	// A consignment number ULP can't carry is left out, not refused.
	n := len(pub.bodies)
	if err := r.Unsigned(polder, edi.Status{Reference: "MF-0007-L1", Meaning: edi.Departed, Code: "13", Time: at, CarrierRef: "", Sender: polder}); err != nil {
		t.Fatal(err)
	}
	if e := pub.bodies[n].(*intents.TnTEventBody); e.DerivedFrom[0].Reference != "" {
		t.Errorf("reference %q", e.DerivedFrom[0].Reference)
	}
	pub.fail = errors.New("closed")
	if err := r.Unsigned(polder, edi.Status{Reference: "MF-0007-L1", Meaning: edi.Departed, Code: "13", Time: at, Sender: polder}); err == nil {
		t.Error("a publish failure")
	}
	if refOK("") || refOK("-x") || refOK("a.b") || !refOK("PH4471") {
		t.Error("refOK")
	}
}
