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
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/edi"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/intents"
)

// TestRelayLegOf: the relay knows the leg each record it re-issued came
// from (signed events, unsigned statuses, alerts), and nothing else; a
// status naming a place off its own leg is re-issued at its leg's place.
func TestRelayLegOf(t *testing.T) {
	r, pub, _ := relayed(t)
	_, _ = r.Orders.Bookings.Update(meridian, "MF-0007", func(b *store.Booking) error {
		b.Order.Legs = append([]store.LegRef{{Mode: intents.ModeRoad, Carrier: polder, Name: "Polder Haulage", Ref: "MF-0007-L1",
			From: "NLTLB", To: "NLRTM", Unsigned: true}}, b.Order.Legs...)
		return nil
	})
	now := time.Now().UTC().Truncate(time.Minute)
	// Polder (leg 1) claims its pickup happened at Santos, a signed leg's port.
	if err := r.Unsigned(polder, edi.Status{Reference: "MF-0007-L1", Meaning: edi.PickedUp, Code: "12", Time: now, Sender: polder, UNLocode: "BRSSZ"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Event(shipLeg, event("EV-1", "est")); err != nil {
		t.Fatal(err)
	}
	if err := r.Alert(shipLeg, delay("AL-1", "EV-1", "2026-10-23T14:00:00Z", "2026-10-26T14:00:00Z")); err != nil {
		t.Fatal(err)
	}
	status := pub.bodies[0].(*intents.TnTEventBody)
	if status.UNLocationCode != "NLTLB" {
		t.Errorf("an unsigned status put at %s", status.UNLocationCode)
	}
	shipEvent := pub.bodies[1].(*intents.TnTEventBody)
	var alertID string
	for _, b := range pub.bodies[2:] {
		if a, ok := b.(*intents.TnTAlertBody); ok {
			alertID = a.AlertID
		}
	}
	for id, want := range map[string]int{status.EventID: 1, shipEvent.EventID: 2, alertID: 2} {
		if leg, ok := r.LegOf(id); !ok || leg != want {
			t.Errorf("%s: leg %d %v, want %d", id, leg, ok, want)
		}
	}
	if _, ok := r.LegOf("MF-EV-999999"); ok {
		t.Error("a leg for a record the relay never re-issued")
	}
}
