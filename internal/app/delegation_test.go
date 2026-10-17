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

	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/internal/shipper"
	"github.com/go-steer/supercargo/pkg/delegation"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
)

const (
	valeVerde = "vale-verde.example"
	serraTr   = "serra-transportes.example"
	aurora    = "mercado-aurora.example"
	mallory   = "mallory.example"
)

// The FTL rig: the farm cooperative books Serra's truck; the supermarket's
// DC (a receiver only) follows the leg by the cooperative's delegation;
// Mallory is anyone else.
var delegationRig = map[string]string{
	valeVerde: "name: Vale Verde\nrole: shipper\n",
	serraTr:   "name: Serra Transportes\nrole: operator\noperator: { refPrefix: SR-BKG, road: { transit: 30h } }\n",
	// The DC takes grants from its suppliers (Mallory is one: still not
	// the shipper of Vale Verde's bookings).
	aurora:  "name: Mercado Aurora\nroles: [receiver]\ngrants: { from: [vale-verde.example, mallory.example] }\n",
	mallory: "name: Mallory\nrole: shipper\n",
}

func until(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for range 200 {
		if ok() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestDelegatedAccess: the shipper grants the DC its truck leg; the DC
// subscribes with the carrier by the grant, receives the leg's signed
// events and asks about it. A grant someone else issued for the booking
// gets the DC nothing from the carrier.
func TestDelegatedAccess(t *testing.T) {
	apps := startNodes(t, delegationRig)
	vv, sr, ma := apps[valeVerde], apps[serraTr], apps[aurora]
	ctx := context.Background()
	conf, err := vv.Shipper.BookRoad(ctx, serraTr, &intents.RoadBookingRequestBody{
		ShipperRef: "VV-FTL-1", Shipper: intents.Party{Name: "Vale Verde", Domain: valeVerde},
		Pickup:       intents.Stop{Facility: "Fazenda Boa Safra", UNLocationCode: "BRSAO"},
		Delivery:     intents.Stop{Facility: "Mercado Aurora DC", UNLocationCode: "BRGRU"},
		Equipment:    []intents.Equipment{{ISOType: "45R1", Count: 1, GrossWeightKg: 26000}},
		Cargo:        intents.Cargo{Description: "Chilled beef", HSCode: "0201"},
		Subscription: &intents.Subscription{Endpoint: vv.Shipper.Endpoint, Intents: []string{intents.TnTEvent, intents.TnTAlert}},
	})
	if err != nil || conf.Status != intents.StatusConfirmed {
		t.Fatalf("%+v %v", conf, err)
	}
	ref := conf.BookingRef

	// The DC can't follow it before it's granted.
	if _, err := ma.Shipper.Ask(ctx, serraTr, ref, "Where is the truck?"); !errors.Is(err, shipper.ErrNotOurBooking) {
		t.Fatalf("asked before the grant: %v", err)
	}

	// The carrier's copy of the booking from before the grant, as the
	// simulator holds it from when the leg started.
	stale, _ := sr.Carrier.Bookings.Get(serraTr, ref)
	g, err := vv.Shipper.Grant(ctx, serraTr, ref, aurora, []string{delegation.ScopeRead, delegation.ScopeSubscribe}, 7*24*time.Hour, "Your DC's delivery")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := vv.Customer.Bookings.Get(serraTr, ref)
	if len(b.Grants) != 1 || b.Grants[0].TokenID != g.TokenID || b.Grants[0].Grantee != aurora {
		t.Errorf("grants recorded %+v", b.Grants)
	}
	until(t, "the DC's subscription", func() bool {
		b, err := ma.Customer.Bookings.Get(serraTr, ref)
		return err == nil && b.Delegated != nil && b.Delegated.SubscriptionID != ""
	})
	held, _ := ma.Customer.Bookings.Get(serraTr, ref)
	if held.Shipper != valeVerde || held.Road == nil || held.Road.Request.Delivery.UNLocationCode != "BRGRU" || held.Delegated.Note != "Your DC's delivery" {
		t.Errorf("the DC holds %+v", held)
	}
	carried, _ := sr.Carrier.Bookings.Get(serraTr, ref)
	if len(carried.Delegates) != 1 || carried.Delegates[0].Grantee != aurora || carried.Delegates[0].TokenID != g.TokenID {
		t.Fatalf("the carrier's delegates %+v", carried.Delegates)
	}

	// The truck leaves: the shipper and the DC both get the signed event.
	ev := &intents.TnTEventBody{EventID: "SR-EV-1", EventType: intents.EventTypeTransport, EventClassifierCode: intents.ClassifierActual,
		EventTypeCode: "DEPA", EventDateTime: time.Now().UTC().Truncate(time.Second).Format(envelope.TimeLayout),
		UNLocationCode: "BRSAO", FacilityName: "Fazenda Boa Safra", Description: "Truck departed the farm"}
	if err := sr.Publisher.Publish(stale, ev); err != nil {
		t.Fatal(err)
	}
	until(t, "the event at the DC and the shipper", func() bool {
		return len(ma.Customer.Events.List(serraTr, ref)) == 1 && len(vv.Customer.Events.List(serraTr, ref)) == 1
	})

	// The DC asks about the leg, by the grant.
	ans, err := ma.Shipper.Ask(ctx, serraTr, ref, "When does the truck arrive?")
	if err != nil || ans.Response.Answer == "" {
		t.Fatalf("the DC's question: %+v %v", ans, err)
	}

	// Mallory issues its own "grant" for the booking to the DC: the DC
	// keeps it (it is Mallory's word), but the carrier gives nothing for
	// it: the booking's shipper is Vale Verde.
	ml := apps[mallory]
	forged, err := delegation.New(mallory, serraTr, aurora, "SR-BKG-0001", []string{delegation.ScopeRead, delegation.ScopeSubscribe}, time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	conf2, err := vv.Shipper.BookRoad(ctx, serraTr, &intents.RoadBookingRequestBody{ShipperRef: "VV-FTL-2", Shipper: intents.Party{Name: "Vale Verde", Domain: valeVerde},
		Pickup: intents.Stop{Facility: "Fazenda Boa Safra", UNLocationCode: "BRSAO"}, Delivery: intents.Stop{Facility: "Mercado Aurora DC", UNLocationCode: "BRGRU"},
		Equipment: []intents.Equipment{{ISOType: "45R1", Count: 1, GrossWeightKg: 26000}}, Cargo: intents.Cargo{Description: "Chilled beef", HSCode: "0201"}})
	if err != nil {
		t.Fatal(err)
	}
	forged.BookingRef = conf2.BookingRef
	token, err := ml.Node.SealDelegation(forged)
	if err != nil {
		t.Fatal(err)
	}
	h := envelope.NewHeader(intents.DelegationGrant, mallory, aurora, time.Now())
	h.BookingRef = &conf2.BookingRef
	if _, err := ml.Node.Exchange(node.WithDelegation(ctx, token), h, &intents.DelegationGrantBody{}); err != nil {
		t.Fatalf("handing the forged grant: %v", err)
	}
	until(t, "the DC to hold Mallory's grant", func() bool {
		b, err := ma.Customer.Bookings.Get(serraTr, conf2.BookingRef)
		return err == nil && b.Delegated != nil
	})
	_, err = ma.Shipper.Subscribe(ctx, serraTr, conf2.BookingRef, ma.Shipper.Endpoint)
	var re *node.RemoteError
	if !errors.As(err, &re) || re.Code != intents.CodeForbidden {
		t.Errorf("subscribing by Mallory's grant: %v", err)
	}
	if _, err := ma.Shipper.Ask(ctx, serraTr, conf2.BookingRef, "Where is it?"); !errors.As(err, &re) || re.Code != intents.CodeForbidden {
		t.Errorf("asking by Mallory's grant: %v", err)
	}
	if b, _ := sr.Carrier.Bookings.Get(serraTr, conf2.BookingRef); len(b.Delegates) != 0 {
		t.Errorf("the carrier took Mallory's grant: %+v", b.Delegates)
	}
	// Mallory can't grant what isn't its booking through the shipper's
	// own step, either.
	if _, err := ml.Shipper.Grant(ctx, serraTr, ref, aurora, []string{delegation.ScopeRead}, time.Hour, ""); !errors.Is(err, shipper.ErrNotGrantable) {
		t.Errorf("Mallory granting: %v", err)
	}
	// A grant to someone who already holds the booking as its shipper is
	// refused by that node.
	if _, err := ml.Shipper.Grant(ctx, serraTr, ref, valeVerde, []string{delegation.ScopeRead}, time.Hour, ""); err == nil {
		t.Error("granted Vale Verde its own booking")
	}
}
