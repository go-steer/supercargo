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
	"fmt"
	"strings"
	"testing"

	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/intents"
)

// TestProposalSuperseded: a later delay of the same leg replaces the open
// proposal, which can't be confirmed any more.
func TestProposalSuperseded(t *testing.T) {
	r, _, _ := relayed(t)
	_ = r.Event(shipLeg, event("EV-1", "est"))
	_ = r.Alert(shipLeg, delay("AL-1", "EV-1", "2026-10-23T14:00:00Z", "2026-10-26T14:00:00Z"))
	_ = r.Alert(shipLeg, delay("AL-2", "EV-1", "2026-10-26T14:00:00Z", "2026-10-26T22:00:00Z"))
	ps := r.Proposals("MF-0007")
	if len(ps) != 2 || ps[0].Status != ProposalSuperseded || ps[1].Status != ProposalOpen ||
		ps[1].Amendment.Pickup.Earliest != "2026-10-28T04:00:00Z" {
		t.Fatalf("proposals %+v", ps)
	}
	if _, err := r.Confirm(context.Background(), ps[0].ID); !errors.Is(err, ErrProposalClosed) {
		t.Errorf("confirming the superseded one: %v", err)
	}
}

// TestProposalCap: closed proposals make room; at most MaxProposals are
// open, and past that, none is made (and the caller hears why).
func TestProposalCap(t *testing.T) {
	r, _, _ := relayed(t)
	r.proposals = map[string]*Proposal{}
	for i := range MaxProposals {
		r.proposals[fmt.Sprintf("X-%d", i)] = &Proposal{ID: fmt.Sprintf("X-%d", i), OrderRef: "MF-0001", Leg: i, Status: ProposalConfirmed}
	}
	_ = r.Event(shipLeg, event("EV-1", "est"))
	if err := r.Alert(shipLeg, delay("AL-1", "EV-1", "2026-10-23T14:00:00Z", "2026-10-26T14:00:00Z")); err != nil || len(r.Proposals("MF-0007")) != 1 {
		t.Fatalf("with closed ones in the way: %v", err)
	}
	for i := range MaxProposals {
		r.proposals[fmt.Sprintf("X-%d", i)] = &Proposal{ID: fmt.Sprintf("X-%d", i), OrderRef: "MF-0001", Leg: i, Status: ProposalOpen}
	}
	if err := r.Alert(shipLeg, delay("AL-2", "EV-1", "2026-10-26T14:00:00Z", "2026-10-27T14:00:00Z")); err == nil {
		t.Error("past the cap: no error")
	}
}

// TestProposalDeadline: a re-timing the last truck can't fit in the
// consignee's date says so, and widens that window only then.
func TestProposalDeadline(t *testing.T) {
	r, _, _ := relayed(t)
	_, _ = r.Orders.Bookings.Update(meridian, "MF-0007", func(b *store.Booking) error {
		b.Order.Request.PlaceOfDelivery.Latest = "2026-10-28T00:00:00Z"
		return nil
	})
	_ = r.Event(shipLeg, event("EV-1", "est"))
	_ = r.Alert(shipLeg, delay("AL-1", "EV-1", "2026-10-23T14:00:00Z", "2026-10-26T14:00:00Z"))
	p := r.Proposals("MF-0007")[0]
	if p.Amendment.Delivery == nil || p.Amendment.Delivery.Latest != "2026-10-29T02:00:00Z" || !strings.Contains(p.Why, "misses the order's delivery date") {
		t.Errorf("proposal %+v", p)
	}
}

// TestRelayBounds: an absurd delay isn't projected; the leg's part of a
// description survives clipping; the oldest relayed events are forgotten
// first.
func TestRelayBounds(t *testing.T) {
	r, pub, _ := relayed(t)
	_ = r.Event(shipLeg, event("EV-1", strings.Repeat("x", 280)))
	if d := pub.bodies[0].(*intents.TnTEventBody).Description; !strings.HasSuffix(d, " (leg 1, Ocean Blue)") || len([]rune(d)) > 280 {
		t.Errorf("description %q", d)
	}
	if err := r.Alert(shipLeg, delay("AL-1", "EV-1", "2026-10-23T14:00:00Z", "2100-01-01T00:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if a := pub.bodies[1].(*intents.TnTAlertBody); a.Kind != intents.AlertException || a.NewETA != "" || len(r.Proposals("MF-0007")) != 0 {
		t.Errorf("an absurd delay: %+v", a)
	}
	for i := range MaxRelayed {
		r.remember(legEvent{"c", "r", fmt.Sprint(i)}, "x")
	}
	if _, ok := r.relayed[legEvent{"ocean-blue.example", "OB-BKG-1042", "EV-1"}]; ok || len(r.relayed) != MaxRelayed {
		t.Errorf("not the oldest forgotten: %d held", len(r.relayed))
	}
	r.remember(legEvent{"c", "r", "0"}, "y") // again: no second entry
	if len(r.order) != MaxRelayed {
		t.Errorf("%d in order", len(r.order))
	}
}
