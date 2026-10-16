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
	"strings"
	"testing"

	"github.com/go-steer/supercargo/internal/store"
)

// TestOrdersResume: after a restart, order references number on after the
// highest order held.
func TestOrdersResume(t *testing.T) {
	o, _ := desk(t)
	_ = o.Bookings.Add(store.Booking{Ref: "MF-0041", Operator: meridian, Order: &store.OrderBooking{}})
	_ = o.Bookings.Add(store.Booking{Ref: "MF-0099", Operator: meridian}) // not an order
	_ = o.Bookings.Add(store.Booking{Ref: "MF-X", Operator: meridian, Order: &store.OrderBooking{}})
	reply, err := o.Handle(context.Background(), orderFrom(cymbal, anOrder()))
	if err != nil || reply.BookingRef != "MF-0042" {
		t.Errorf("resumed at %+v, %v", reply, err)
	}
}

// TestRelayResume: after a restart, the relay's event, alert and proposal
// IDs number on after the order events held; others' IDs don't count.
func TestRelayResume(t *testing.T) {
	r, _, _ := relayed(t)
	r.Resume([]string{"MF-EV-000041", "MF-AL-000007", "EV-000099", "XX-EV-000500", "MF-EV-12x", "MF"})
	if id := r.nextID("EV"); id != "MF-EV-000042" {
		t.Errorf("next %s", id)
	}
}

// failing is a Saver that refuses every write.
type failing struct{}

func (failing) Put(string, any) error                 { return errors.New("disk full") }
func (failing) DeleteBefore(string) error             { return errors.New("disk full") }
func (failing) Load(func(string, []byte) error) error { return nil }

// TestBookRecordFails: a leg booked but not recorded (the file refused)
// is withdrawn from its carrier.
func TestBookRecordFails(t *testing.T) {
	orch, _ := orchestrated(t, &carriers{}, lane)
	c := &cancels{}
	orch.Cancel = c
	if _, err := orch.Orders.Handle(context.Background(), orderFrom(cymbal, anOrder())); err != nil {
		t.Fatal(err)
	}
	if err := orch.Orders.Bookings.Persist(failing{}); err != nil {
		t.Fatal(err)
	}
	_, err := orch.Book(context.Background(), "MF-0007")
	if err == nil || !strings.Contains(err.Error(), "withdrawn") || len(c.got) != 1 {
		t.Errorf("not recorded: %v, cancelled %v", err, c.got)
	}
}
