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

package store

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/go-steer/supercargo/pkg/intents"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

func (c *clock) advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func newClock() *clock { return &clock{t: time.Date(2026, 10, 9, 14, 0, 0, 0, time.UTC)} }

func TestReplay(t *testing.T) {
	clk := newClock()
	r := NewReplay(ReplayOptions{Retention: time.Hour, Now: clk.now})
	if err := r.Record("a.example", "m1"); err != nil {
		t.Fatal(err)
	}
	if err := r.Record("a.example", "m1"); !errors.Is(err, ErrReplay) {
		t.Errorf("second Record = %v, want ErrReplay", err)
	}
	// The key is (sender, messageId): another sender may use the same ID.
	if err := r.Record("b.example", "m1"); err != nil {
		t.Errorf("same ID, other sender: %v", err)
	}
	clk.advance(59 * time.Minute)
	if err := r.Record("a.example", "m1"); !errors.Is(err, ErrReplay) {
		t.Errorf("just inside retention: %v, want ErrReplay", err)
	}
	clk.advance(time.Minute)
	if err := r.Record("a.example", "m1"); err != nil {
		t.Errorf("after retention: %v", err)
	}
	if r.Retention() != time.Hour {
		t.Errorf("Retention = %v", r.Retention())
	}
}

func TestReplayFullFailsClosed(t *testing.T) {
	clk := newClock()
	r := NewReplay(ReplayOptions{Retention: time.Hour, Max: 2, Now: clk.now})
	_ = r.Record("a.example", "m1")
	_ = r.Record("b.example", "m2")
	if err := r.Record("c.example", "m3"); !errors.Is(err, ErrFull) {
		t.Errorf("Record into a full store = %v, want ErrFull", err)
	}
	if err := r.Record("a.example", "m1"); !errors.Is(err, ErrReplay) {
		t.Errorf("replay into a full store = %v, want ErrReplay", err)
	}
	clk.advance(time.Hour)
	if err := r.Record("c.example", "m3"); err != nil {
		t.Errorf("Record after expiry: %v", err)
	}
	if r.Len() != 1 {
		t.Errorf("Len = %d after expiry, want 1", r.Len())
	}
}

// TestReplayPerSenderQuota: one sender filling its allowance doesn't lock
// anyone else out.
func TestReplayPerSenderQuota(t *testing.T) {
	clk := newClock()
	r := NewReplay(ReplayOptions{Retention: time.Hour, PerSender: 3, Now: clk.now})
	for i := range 3 {
		if err := r.Record("noisy.example", fmt.Sprint(i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Record("noisy.example", "4"); !errors.Is(err, ErrFull) {
		t.Errorf("over quota: %v, want ErrFull", err)
	}
	if err := r.Record("partner.example", "1"); err != nil {
		t.Errorf("another sender while one is over quota: %v", err)
	}
	clk.advance(time.Hour)
	if err := r.Record("noisy.example", "5"); err != nil {
		t.Errorf("after the sender's entries expire: %v", err)
	}
}

// TestReplayExpiresInOrder: expiry walks the queue, so entries go exactly
// when their time is up, and the queue's spent front is reclaimed.
func TestReplayExpiresInOrder(t *testing.T) {
	clk := newClock()
	r := NewReplay(ReplayOptions{Retention: time.Minute, Now: clk.now})
	for i := range 3000 {
		_ = r.Record("a.example", fmt.Sprint("old", i))
	}
	clk.advance(30 * time.Second)
	for i := range 10 {
		_ = r.Record("a.example", fmt.Sprint("new", i))
	}
	clk.advance(30 * time.Second) // the old ones expire, the new ones don't
	_ = r.Record("b.example", "x")
	if n := r.Len(); n != 11 {
		t.Errorf("Len = %d, want 11 (10 new + 1)", n)
	}
	r.mu.Lock()
	live := len(r.queue) - r.head
	r.mu.Unlock()
	if live != 11 {
		t.Errorf("queue holds %d live entries, want 11", live)
	}
	if err := r.Record("a.example", "new3"); !errors.Is(err, ErrReplay) {
		t.Errorf("an unexpired entry was dropped: %v", err)
	}
}

func TestReplayDefaults(t *testing.T) {
	r := NewReplay(ReplayOptions{Retention: -1, Max: -1, PerSender: -1})
	if r.Retention() != 15*time.Minute || r.opts.Max != 1_000_000 || r.opts.PerSender != 100_000 {
		t.Errorf("defaults: %+v", r.opts)
	}
	if err := r.Record("a.example", "m"); err != nil {
		t.Error(err)
	}
}

func TestReplayConcurrent(t *testing.T) {
	r := NewReplay(ReplayOptions{})
	var wg sync.WaitGroup
	accepted := make(chan struct{}, 100)
	for range 100 {
		wg.Go(func() {
			if r.Record("a.example", "same") == nil {
				accepted <- struct{}{}
			}
		})
	}
	wg.Wait()
	close(accepted)
	if n := len(accepted); n != 1 {
		t.Errorf("%d concurrent deliveries of one message accepted, want 1", n)
	}
}

func booking(operator, ref string) Booking {
	setPoint := 4.0
	return Booking{
		Ref: ref, Shipper: "a.example", Operator: operator,
		Request: intents.BookingRequestBody{
			Equipment:    []intents.Equipment{{ISOType: "45G1", Count: 1}},
			Consignee:    &intents.Consignee{Name: "C"},
			Subscription: &intents.Subscription{Endpoint: "https://a.example/ulp", Intents: []string{"ulp.tnt.event"}},
		},
		Confirmation: intents.BookingConfirmationBody{
			EBL:      &intents.EBL{VesselName: "V", Containers: []intents.ShippingContainer{{ContainerNumber: "OCBU0000010", IsReefer: true, TargetTemperatureCelsius: &setPoint}}},
			Messages: []intents.Message{{Code: "c", Detail: "d"}},
		},
	}
}

func TestBookings(t *testing.T) {
	s := NewBookings()
	if _, err := s.Get("ob.example", "OB-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(missing) = %v", err)
	}
	if err := s.Add(booking("ob.example", "OB-1")); err != nil {
		t.Fatal(err)
	}
	// The same reference from another operator is another booking.
	if err := s.Add(booking("other.example", "OB-1")); err != nil {
		t.Errorf("same ref, other operator: %v", err)
	}
	if err := s.Add(booking("ob.example", "OB-1")); !errors.Is(err, ErrExists) {
		t.Errorf("Add over an existing booking = %v, want ErrExists", err)
	}
	if s.Len() != 2 {
		t.Errorf("Len = %d", s.Len())
	}
	if b, err := s.Get("other.example", "OB-1"); err != nil || b.Operator != "other.example" {
		t.Errorf("Get = %+v, %v", b, err)
	}
}

// TestBookingsCopyDeeply: nothing the caller holds aliases the store.
func TestBookingsCopyDeeply(t *testing.T) {
	s := NewBookings()
	in := booking("ob.example", "OB-1")
	_ = s.Add(in)
	in.Request.Equipment[0].Count = 99
	in.Request.Subscription.Intents[0] = "changed"
	out, _ := s.Get("ob.example", "OB-1")
	out.Request.Consignee.Name = "changed"
	out.Confirmation.EBL.VesselName = "changed"
	out.Confirmation.EBL.Containers[0].ContainerNumber = "changed"
	*out.Confirmation.EBL.Containers[0].TargetTemperatureCelsius = -20
	out.Confirmation.Messages[0].Code = "changed"
	again, _ := s.Get("ob.example", "OB-1")
	if again.Request.Equipment[0].Count != 1 || again.Request.Subscription.Intents[0] != "ulp.tnt.event" ||
		again.Request.Consignee.Name != "C" || again.Confirmation.EBL.VesselName != "V" ||
		again.Confirmation.EBL.Containers[0].ContainerNumber != "OCBU0000010" ||
		*again.Confirmation.EBL.Containers[0].TargetTemperatureCelsius != 4 || again.Confirmation.Messages[0].Code != "c" {
		t.Errorf("the store shares data with its callers: %+v", again)
	}
}
