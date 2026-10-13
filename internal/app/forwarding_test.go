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

package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/config"
	"github.com/go-steer/supercargo/internal/forwarder"
	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/pkg/intents"
)

// startForwarding starts Cymbal Shoes (a shipper) and Meridian (a
// forwarder: operator toward Cymbal, shipper toward its carriers).
func startForwarding(t *testing.T) (cymbal, meridian *App) {
	t.Helper()
	cert, key, ca := tlsFiles(t, "cymbal-shoes.example", "meridian-forwarding.example")
	cAddr, mAddr := freePort(t), freePort(t)
	tlsYAML := `tls: { mode: local-ca, certFile: "` + cert + `", keyFile: "` + key + `", caFile: "` + ca + `" }
peers: { cymbal-shoes.example: "` + cAddr + `", meridian-forwarding.example: "` + mAddr + `" }
`
	cCfg, err := config.Parse([]byte(`domain: cymbal-shoes.example
name: Cymbal Shoes
role: shipper
listen: "` + cAddr + `"
keys: [{ kid: cy-1, privateKeyFile: "` + keyFile(t) + `" }]
partners: [meridian-forwarding.example]
` + tlsYAML))
	if err != nil {
		t.Fatal(err)
	}
	mCfg, err := config.Parse([]byte(`domain: meridian-forwarding.example
name: Meridian Forwarding
roles: [operator, shipper]
listen: "` + mAddr + `"
keys: [{ kid: mf-1, privateKeyFile: "` + keyFile(t) + `" }]
partners: [cymbal-shoes.example]
forwarder: { refPrefix: MF }
` + tlsYAML))
	if err != nil {
		t.Fatal(err)
	}
	var apps []*App
	for _, c := range []*config.Config{cCfg, mCfg} {
		a, err := New(c, quiet())
		if err != nil {
			t.Fatal(err)
		}
		apps = append(apps, a)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- a.Serve(ctx) }()
		t.Cleanup(func() { cancel(); <-done })
		waitListening(t, c.Listen)
	}
	return apps[0], apps[1]
}

func order() *intents.ForwardingRequestBody {
	return &intents.ForwardingRequestBody{
		ShipperRef: "CY-PO-55120",
		Incoterms:  intents.Incoterms{Rule: "DAP", Version: "2020", NamedPlace: "Cymbal Calçados DC, Cajamar"},
		Shipper:    intents.Party{Name: "Cymbal Shoes B.V.", Domain: "cymbal-shoes.example"},
		Consignee:  intents.Party{Name: "Cymbal Calçados Ltda"},
		PlaceOfReceipt: intents.Place{Type: intents.PlaceDoor, UNLocationCode: "NLTLB",
			Address: &intents.Address{Street: "Ringbaan Oost 100", Locality: "Tilburg", Country: "NL"}},
		PlaceOfDelivery: intents.Place{Type: intents.PlaceDoor, Facility: "Cymbal Calçados DC, dock 2", UNLocationCode: "BRSAO", Latest: "2026-10-27T12:00:00Z",
			Address: &intents.Address{Street: "Rodovia Anhanguera, km 33", Locality: "Cajamar", Region: "SP", Country: "BR"}},
		Equipment:    []intents.Equipment{{ISOType: "45G1", Count: 1, GrossWeightKg: 12000}},
		Cargo:        intents.Cargo{Description: "Footwear (sneakers)", HSCode: "6404"},
		Subscription: &intents.Subscription{Endpoint: "https://cymbal-shoes.example/ulp", Intents: []string{intents.TnTEvent, intents.TnTAlert}},
	}
}

func plan() *intents.ForwardingConfirmationBody {
	return &intents.ForwardingConfirmationBody{Status: intents.StatusConfirmed, PlannedDelivery: "2026-10-26T10:00:00Z",
		Legs: []intents.PlannedLeg{
			{Mode: intents.ModeRoad, From: "NLTLB", To: "NLRTM"},
			{Mode: intents.ModeOcean, From: "NLRTM", To: "BRSSZ", Carrier: "Ocean Blue", Vessel: "Ocean Blue Aurora 42W"},
			{Mode: intents.ModeRoad, From: "BRSSZ", To: "BRSAO", Carrier: "Serra Transportes"},
		}}
}

// TestForwardingOrder: an order is taken at once as pending, with its
// reference and subscription, and recorded on both sides; Meridian's
// person confirms the plan, which reaches Cymbal as a signed answer to
// the order, once.
func TestForwardingOrder(t *testing.T) {
	cymbal, meridian := startForwarding(t)
	ctx := context.Background()
	pending, err := cymbal.Shipper.Order(ctx, "meridian-forwarding.example", order())
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != intents.StatusPending || pending.OrderRef != "MF-0007" || pending.SubscriptionID != "SUB-MF-0007" {
		t.Fatalf("answer %+v", pending)
	}
	mine, err := cymbal.Customer.Bookings.Get("meridian-forwarding.example", "MF-0007")
	if err != nil || mine.Order == nil || mine.Order.MessageID == "" {
		t.Fatalf("Cymbal's record %+v, %v", mine, err)
	}
	theirs, err := meridian.Carrier.Bookings.Get("meridian-forwarding.example", "MF-0007")
	if err != nil || theirs.Order == nil || theirs.Shipper != "cymbal-shoes.example" || theirs.Order.MessageID != mine.Order.MessageID {
		t.Fatalf("Meridian's record %+v, %v", theirs, err)
	}
	// A plan that doesn't reach the place of delivery is caught before
	// anything is sent.
	short := plan()
	short.Legs = short.Legs[:2]
	if err := meridian.Orders.Answer(ctx, "MF-0007", short); !errors.Is(err, forwarder.ErrInvalidAnswer) {
		t.Errorf("a plan ending in Santos: %v", err)
	}
	if err := meridian.Orders.Answer(ctx, "MF-0007", plan()); err != nil {
		t.Fatal(err)
	}
	mine, _ = cymbal.Customer.Bookings.Get("meridian-forwarding.example", "MF-0007")
	if c := mine.Order.Confirmation; c.Status != intents.StatusConfirmed || len(c.Legs) != 3 || c.SubscriptionID != "SUB-MF-0007" {
		t.Errorf("Cymbal holds %+v", c)
	}
	if theirs, _ := meridian.Carrier.Bookings.Get("meridian-forwarding.example", "MF-0007"); theirs.Order.Confirmation.Status != intents.StatusConfirmed {
		t.Error("Meridian didn't record its answer")
	}
	if err := meridian.Orders.Answer(ctx, "MF-0007", plan()); !errors.Is(err, forwarder.ErrAnswered) {
		t.Errorf("a second answer: %v", err)
	}
	if err := meridian.Orders.Answer(ctx, "MF-9999", plan()); !errors.Is(err, forwarder.ErrUnknownOrder) {
		t.Errorf("an unknown order: %v", err)
	}
}

// TestForwardingAnswerRefused: Cymbal takes a forwarder's later answer
// only in reply to an order it sent that forwarder, still pending; every
// other answer gets the same refusal, and changes nothing.
func TestForwardingAnswerRefused(t *testing.T) {
	cymbal, meridian := startForwarding(t)
	ctx := context.Background()
	if _, err := cymbal.Shipper.Order(ctx, "meridian-forwarding.example", order()); err != nil {
		t.Fatal(err)
	}
	theirs, _ := meridian.Carrier.Bookings.Get("meridian-forwarding.example", "MF-0007")
	send := func(ref, inReplyTo string, body *intents.ForwardingConfirmationBody) error {
		h := intents.HeaderFor(intents.ForwardingConfirmation, "meridian-forwarding.example", "cymbal-shoes.example", time.Now())
		h.BookingRef, h.InReplyTo = &ref, &inReplyTo
		body.OrderRef = ref
		if body.Status == intents.StatusConfirmed {
			body.SubscriptionID = "SUB-MF-0007" // as Orders.Answer keeps it
		}
		_, err := meridian.Node.Exchange(ctx, h, body)
		return err
	}
	stillPending := &intents.ForwardingConfirmationBody{Status: intents.StatusPending}
	for name, err := range map[string]error{
		"not in reply to the order":  send("MF-0007", "00000000-0000-4000-8000-000000000009", plan()),
		"an order Cymbal never sent": send("MF-0008", theirs.Order.MessageID, plan()),
		"pending again":              send("MF-0007", theirs.Order.MessageID, stillPending),
	} {
		var re *node.RemoteError
		if !errors.As(err, &re) || re.Code != intents.CodeForbidden {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A plan that misses the order's delivery date is refused as such.
	late := plan()
	late.PlannedDelivery = "2026-10-28T10:00:00Z"
	var re *node.RemoteError
	if err := send("MF-0007", theirs.Order.MessageID, late); !errors.As(err, &re) || re.Code != intents.CodeInvalidBody {
		t.Errorf("a late plan: %v", err)
	}
	if mine, _ := cymbal.Customer.Bookings.Get("meridian-forwarding.example", "MF-0007"); mine.Order.Confirmation.Status != intents.StatusPending {
		t.Errorf("a refused answer changed the order: %+v", mine.Order.Confirmation)
	}
	// The real answer still gets through; the same answer again (its reply
	// lost on the way) is taken without changing anything; nothing else is.
	if err := send("MF-0007", theirs.Order.MessageID, plan()); err != nil {
		t.Fatal(err)
	}
	if err := send("MF-0007", theirs.Order.MessageID, plan()); err != nil {
		t.Errorf("the same answer again: %v", err)
	}
	if err := send("MF-0007", theirs.Order.MessageID, &intents.ForwardingConfirmationBody{Status: intents.StatusRejected}); !errors.As(err, &re) || re.Code != intents.CodeForbidden {
		t.Errorf("an answer after the answer: %v", err)
	}
}
