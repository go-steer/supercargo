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

package operator

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/delegation"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
	"github.com/go-steer/supercargo/pkg/transport/rest"
)

// refusingPusher fails every push, retryably.
type refusingPusher struct {
	mu    sync.Mutex
	tries map[string]int
}

func (f *refusingPusher) Push(_ context.Context, h envelope.Header, _ intents.Body, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tries[h.Receiver]++
	return &rest.TransportError{Receiver: h.Receiver, Err: errors.New("connection refused")}
}

// TestDelegatePushEnds: a delegate's push isn't retried past its grant's
// end; the publisher reads the booking's delegates as stored now, not the
// caller's copy.
func TestDelegatePushEnds(t *testing.T) {
	now := at
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	f := &refusingPusher{tries: map[string]int{}}
	var drops []string
	var dropMu sync.Mutex
	bookings := store.NewBookings()
	b := store.Booking{Ref: "OB-BKG-1", Shipper: "cymbal-shoes.example", Operator: "ocean-blue.example", Request: *body()}
	_ = bookings.Add(b)
	// Stored after the caller's copy was taken.
	_, _ = bookings.Update("ocean-blue.example", "OB-BKG-1", func(x *store.Booking) error {
		x.Delegates = []store.Delegate{{Grantee: dc, Subscription: intents.Subscription{Endpoint: "https://" + dc + "/ulp", Intents: []string{intents.TnTEvent}},
			Expires: at.Add(30 * time.Millisecond)}}
		return nil
	})
	pub := &Publisher{Pusher: f, Domain: "ocean-blue.example", Log: store.NewEventLog(0), Now: clock, Bookings: bookings,
		Backoff: []time.Duration{20 * time.Millisecond, 20 * time.Millisecond, 20 * time.Millisecond, 20 * time.Millisecond},
		OnDrop:  func(_, id string, err error) { dropMu.Lock(); drops = append(drops, err.Error()); dropMu.Unlock() }}
	defer pub.Close()
	ev := &intents.TnTEventBody{EventID: "EV-1", EventType: intents.EventTypeTransport, EventClassifierCode: intents.ClassifierActual,
		EventTypeCode: "DEPA", EventDateTime: "2026-10-09T15:00:00Z", UNLocationCode: "NLRTM", FacilityName: "Rotterdam", Description: "Departed"}
	if err := pub.Publish(b, ev); err != nil { // b: the stale copy, no delegates
		t.Fatal(err)
	}
	time.Sleep(25 * time.Millisecond)
	mu.Lock()
	now = at.Add(time.Hour) // the grant ends during the retries
	mu.Unlock()
	for range 100 {
		dropMu.Lock()
		n := len(drops)
		dropMu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.mu.Lock()
	tries := f.tries[dc]
	f.mu.Unlock()
	dropMu.Lock()
	defer dropMu.Unlock()
	if tries == 0 || tries > 3 || len(drops) != 1 || !strings.Contains(drops[0], "delegation ended") {
		t.Errorf("%d tries, drops %v", tries, drops)
	}
}

// TestDelegateQueueFull: a delegate's push not queued is OnDrop's; the
// publish of the shipper's succeeds.
func TestDelegateQueueFull(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	var drops int
	var mu sync.Mutex
	pub := &Publisher{Pusher: blocking(block), Domain: "ocean-blue.example", Log: store.NewEventLog(0), Now: func() time.Time { return at }, QueueLen: 1,
		OnDrop: func(string, string, error) { mu.Lock(); drops++; mu.Unlock() }}
	defer pub.Close()
	b := store.Booking{Ref: "OB-BKG-1", Shipper: "cymbal-shoes.example", Operator: "ocean-blue.example", Request: *body(),
		Delegates: []store.Delegate{{Grantee: dc, Subscription: intents.Subscription{Endpoint: "https://" + dc + "/ulp", Intents: []string{intents.TnTEvent}}, Expires: at.Add(time.Hour)}}}
	b.Request.Subscription = &intents.Subscription{Endpoint: "https://cymbal-shoes.example/ulp", Intents: []string{intents.TnTEvent}}
	for i := range 4 {
		ev := &intents.TnTEventBody{EventID: "EV-" + string(rune('1'+i)), EventType: intents.EventTypeTransport, EventClassifierCode: intents.ClassifierActual,
			EventTypeCode: "DEPA", EventDateTime: "2026-10-09T15:00:00Z", UNLocationCode: "NLRTM", FacilityName: "Rotterdam", Description: "Departed"}
		_ = pub.Publish(b, ev)
	}
	mu.Lock()
	defer mu.Unlock()
	if drops == 0 {
		t.Error("no drop reported")
	}
}

type blocking chan struct{}

func (b blocking) Push(ctx context.Context, h envelope.Header, _ intents.Body, _ string) error {
	if h.Receiver == dc {
		select {
		case <-b:
		case <-ctx.Done():
		}
	}
	return nil
}

// TestNoSubscriptionToAnOrder: a forwarding order isn't subscribed to by
// delegation: its legs are.
func TestNoSubscriptionToAnOrder(t *testing.T) {
	bookings := store.NewBookings()
	_ = bookings.Add(store.Booking{Ref: "MF-0007", Shipper: "cymbal-shoes.example", Operator: "ocean-blue.example", Order: &store.OrderBooking{}})
	s := &SubscriptionService{Bookings: bookings, Now: func() time.Time { return at.Add(time.Hour) }}
	d := mint(t, "cymbal-shoes.example", dc, "MF-0007", delegation.ScopeSubscribe)
	var rej *node.Rejection
	if err := s.Authorize(context.Background(), subReq(dc, "MF-0007", "https://"+dc+"/ulp", d, at.Add(time.Hour))); !errors.As(err, &rej) {
		t.Errorf("an order: %v", err)
	}
}
