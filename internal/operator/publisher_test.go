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
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
	"github.com/go-steer/supercargo/pkg/transport/rest"
)

// fakePusher records pushes and answers each with the next scripted error
// (nil once the script runs out).
type fakePusher struct {
	mu      sync.Mutex
	script  []error
	pushed  []string // body IDs, one per attempt
	headers []envelope.Header
	block   chan struct{}
	entered chan struct{} // if set, signalled as each Push starts
}

func (f *fakePusher) Push(ctx context.Context, h envelope.Header, body intents.Body, endpoint string) error {
	if f.entered != nil {
		f.entered <- struct{}{}
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pushed = append(f.pushed, pushID(body))
	f.headers = append(f.headers, h)
	if endpoint != "https://cymbal-shoes.example/ulp" {
		return fmt.Errorf("pushed to %s", endpoint)
	}
	if len(f.script) == 0 {
		return nil
	}
	err := f.script[0]
	f.script = f.script[1:]
	return err
}

func (f *fakePusher) attempts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.pushed...)
}

func subscribed(intentsList ...string) store.Booking {
	return store.Booking{Ref: "OB-BKG-1", Shipper: "cymbal-shoes.example", Operator: "ocean-blue.example",
		Request: intents.BookingRequestBody{Subscription: &intents.Subscription{
			Endpoint: "https://cymbal-shoes.example/ulp", Intents: intentsList}}}
}

func ev(id string) *intents.TnTEventBody {
	return &intents.TnTEventBody{EventID: id, EventType: intents.EventTypeEquipment, EventClassifierCode: intents.ClassifierActual,
		EventCode: "GTIN", EventDateTime: "2026-10-07T11:20:00Z", Location: intents.Location{UNLocode: "NLRTM", Name: "Rotterdam"},
		EquipmentRef: "OCBU0000004"}
}

type drops struct {
	mu  sync.Mutex
	ids []string
}

func (d *drops) add(_, id string, _ error) { d.mu.Lock(); d.ids = append(d.ids, id); d.mu.Unlock() }

func (d *drops) list() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.ids...)
}

func publisher(f *fakePusher, d *drops) *Publisher {
	return &Publisher{Pusher: f, Domain: "ocean-blue.example", Log: store.NewEventLog(0), OnDrop: d.add,
		Backoff: []time.Duration{time.Millisecond, time.Millisecond}}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("condition not reached")
}

func TestPublishInOrder(t *testing.T) {
	f, d := &fakePusher{}, &drops{}
	p := publisher(f, d)
	defer p.Close()
	b := subscribed(intents.TnTEvent)
	for i := range 20 {
		if err := p.Publish(b, ev(fmt.Sprintf("EV-%02d", i))); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, func() bool { return len(f.attempts()) == 20 })
	for i, id := range f.attempts() {
		if id != fmt.Sprintf("EV-%02d", i) {
			t.Fatalf("push %d is %s: out of order", i, id)
		}
	}
	f.mu.Lock()
	h := f.headers[0]
	f.mu.Unlock()
	if h.Sender != "ocean-blue.example" || h.Receiver != "cymbal-shoes.example" || *h.BookingRef != "OB-BKG-1" || h.InReplyTo != nil {
		t.Errorf("push header %+v", h)
	}
}

func TestRetryClassification(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		attempts int // with 2 backoff steps: 1 + 2 retries at most
		dropped  bool
	}{
		{"network error", &rest.TransportError{Err: errors.New("connection refused")}, 3, true},
		{"unclassified error", errors.New("sealing failed"), 1, true},
		{"canceled", &rest.TransportError{Err: context.Canceled}, 1, true},
		{"503", &rest.StatusError{Code: http.StatusServiceUnavailable}, 3, true},
		{"500", &rest.StatusError{Code: http.StatusInternalServerError}, 3, true},
		{"403", &rest.StatusError{Code: http.StatusForbidden}, 1, true},
		{"421", &rest.StatusError{Code: http.StatusMisdirectedRequest}, 1, true},
		{"retryable ulp.error", &node.RemoteError{Code: "internal", Retryable: true}, 3, true},
		{"forbidden ulp.error", &node.RemoteError{Code: "forbidden"}, 1, true},
		{"response failed verification", fmt.Errorf("x: %w", node.ErrBadResponse), 1, true},
		{"endpoint refused by the guard", &rest.TransportError{Err: fmt.Errorf("x: %w", rest.ErrRefused)}, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakePusher{script: []error{tc.err, tc.err, tc.err, tc.err}}
			d := &drops{}
			p := publisher(f, d)
			defer p.Close()
			_ = p.Publish(subscribed(intents.TnTEvent), ev("EV-1"))
			waitFor(t, func() bool { return len(d.list()) == 1 })
			if n := len(f.attempts()); n != tc.attempts {
				t.Errorf("%d attempts, want %d", n, tc.attempts)
			}
		})
	}
	// A transient failure, then success: delivered, not dropped.
	f := &fakePusher{script: []error{&rest.TransportError{Err: errors.New("reset")}}}
	d := &drops{}
	p := publisher(f, d)
	defer p.Close()
	_ = p.Publish(subscribed(intents.TnTEvent), ev("EV-1"))
	waitFor(t, func() bool { return len(f.attempts()) == 2 })
	time.Sleep(10 * time.Millisecond)
	if len(d.list()) != 0 {
		t.Errorf("a push delivered on retry was dropped")
	}
	// Each attempt is a new message.
	f.mu.Lock()
	if f.headers[0].MessageID == f.headers[1].MessageID {
		t.Error("a retry reused the messageId")
	}
	f.mu.Unlock()
}

func TestPublishRejects(t *testing.T) {
	f, d := &fakePusher{}, &drops{}
	p := publisher(f, d)
	if err := p.Publish(subscribed(intents.TnTEvent), &intents.BookingConfirmationBody{BookingRef: "x", Status: "confirmed"}); err == nil {
		t.Error("published a confirmation")
	}
	bad := ev("EV-1")
	bad.EventCode = "LOST"
	if err := p.Publish(subscribed(intents.TnTEvent), bad); err == nil {
		t.Error("published an invalid event")
	}
	// Not subscribed: recorded, not pushed.
	if err := p.Publish(subscribed(intents.TnTAlert), ev("EV-2")); err != nil {
		t.Fatal(err)
	}
	if err := p.Publish(store.Booking{Ref: "OB-BKG-2", Operator: "ocean-blue.example"}, ev("EV-3")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if len(f.attempts()) != 0 {
		t.Errorf("pushed without a subscription for it: %v", f.attempts())
	}
	if len(p.Log.List("ocean-blue.example", "OB-BKG-1")) != 1 || len(p.Log.List("ocean-blue.example", "OB-BKG-2")) != 1 {
		t.Error("unpushed events weren't recorded")
	}
	p.Close()
	if err := p.Publish(subscribed(intents.TnTEvent), ev("EV-4")); !errors.Is(err, ErrClosed) {
		t.Errorf("Publish after Close = %v", err)
	}
	// A full log refuses the record, and so the push.
	p2 := publisher(f, d)
	p2.Log = store.NewEventLog(1)
	defer p2.Close()
	_ = p2.Publish(subscribed(intents.TnTAlert), ev("EV-1"))
	if err := p2.Publish(subscribed(intents.TnTAlert), ev("EV-2")); err == nil {
		t.Error("published past a full log")
	}
}

func TestQueueFullDrops(t *testing.T) {
	f := &fakePusher{block: make(chan struct{}), entered: make(chan struct{}, 4)}
	d := &drops{}
	p := publisher(f, d)
	p.QueueLen = 1
	b := subscribed(intents.TnTEvent)
	_ = p.Publish(b, ev("EV-1")) // taken by the worker, which blocks
	<-f.entered
	_ = p.Publish(b, ev("EV-2")) // fills the queue
	if err := p.Publish(b, ev("EV-3")); err == nil {
		t.Error("Publish into a full queue succeeded")
	}
	if got := d.list(); len(got) != 1 || got[0] != "EV-3" {
		t.Errorf("drops %v, want [EV-3]", got)
	}
	close(f.block)
	p.Close()
}

// TestCloseStopsRetries: Close doesn't wait out a backoff, makes no further
// attempt, and reports the push it abandoned.
func TestCloseStopsRetries(t *testing.T) {
	down := &rest.TransportError{Err: errors.New("down")}
	f := &fakePusher{script: []error{down, down, down}}
	d := &drops{}
	p := &Publisher{Pusher: f, Domain: "ocean-blue.example", Log: store.NewEventLog(0), OnDrop: d.add, Backoff: []time.Duration{time.Hour}}
	_ = p.Publish(subscribed(intents.TnTEvent), ev("EV-1"))
	waitFor(t, func() bool { return len(f.attempts()) == 1 })
	done := make(chan struct{})
	go func() { p.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close waited out the backoff")
	}
	if n := len(f.attempts()); n != 1 {
		t.Errorf("%d attempts after Close, want 1", n)
	}
	if got := d.list(); len(got) != 1 || got[0] != "EV-1" {
		t.Errorf("drops %v, want [EV-1]", got)
	}
}

// TestCloseReportsQueued: pushes still queued at Close are reported.
func TestCloseReportsQueued(t *testing.T) {
	f := &fakePusher{block: make(chan struct{})}
	d := &drops{}
	p := publisher(f, d)
	b := subscribed(intents.TnTEvent)
	_ = p.Publish(b, ev("EV-1")) // taken by the worker, which blocks
	time.Sleep(10 * time.Millisecond)
	_ = p.Publish(b, ev("EV-2"))
	_ = p.Publish(b, ev("EV-3"))
	p.Close()
	if got := d.list(); len(got) != 3 {
		t.Errorf("drops %v, want EV-1..EV-3", got)
	}
}

func TestPublishDuplicateID(t *testing.T) {
	f, d := &fakePusher{}, &drops{}
	p := publisher(f, d)
	defer p.Close()
	b := subscribed(intents.TnTEvent, intents.TnTAlert)
	if err := p.Publish(b, ev("EV-1")); err != nil {
		t.Fatal(err)
	}
	changed := ev("EV-1")
	changed.EventCode = "LOAD"
	if err := p.Publish(b, changed); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("republishing EV-1 = %v, want ErrDuplicate", err)
	}
	// An alert may share an event's ID.
	if err := p.Publish(b, al("EV-1")); err != nil {
		t.Fatalf("an alert sharing an event's ID: %v", err)
	}
	waitFor(t, func() bool { return len(f.attempts()) == 2 })
	time.Sleep(10 * time.Millisecond)
	if n := len(f.attempts()); n != 2 {
		t.Errorf("%d pushes, want 2", n)
	}
}

// TestPublishCopiesBody: changing the caller's struct after Publish changes
// neither the log nor the push.
func TestPublishCopiesBody(t *testing.T) {
	release := make(chan struct{})
	got := make(chan intents.Body, 1)
	p := publisher(nil, &drops{})
	defer p.Close()
	p.Pusher = pusherFunc(func(_ context.Context, _ envelope.Header, body intents.Body, _ string) error {
		<-release
		got <- body
		return nil
	})
	b := subscribed(intents.TnTEvent)
	e := ev("EV-1")
	if err := p.Publish(b, e); err != nil {
		t.Fatal(err)
	}
	e.EventCode = "LOAD"
	close(release)
	select {
	case body := <-got:
		if body.(*intents.TnTEventBody).EventCode != "GTIN" {
			t.Errorf("pushed %+v, want the body as published", body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("nothing pushed")
	}
	if recs := p.Log.List(b.Operator, b.Ref); len(recs) != 1 || !strings.Contains(string(recs[0].Body), "GTIN") {
		t.Errorf("logged %+v", recs)
	}
}

func TestPublishRejectsOtherIntents(t *testing.T) {
	p := publisher(&fakePusher{}, &drops{})
	defer p.Close()
	// A valid body of an intent that isn't pushed.
	body := &intents.ErrorBody{Code: "internal", Detail: "x"}
	if err := body.Validate(); err != nil {
		t.Fatalf("test body invalid: %v", err)
	}
	if err := p.Publish(subscribed(intents.Error), body); err == nil || !strings.Contains(err.Error(), "not something to publish") {
		t.Errorf("published a ulp.error: %v", err)
	}
}

// TestWorkersExitAndAreCapped: a worker leaves once its queue is empty, and
// no more than MaxWorkers subscriptions have pushes in flight.
func TestWorkersExitAndAreCapped(t *testing.T) {
	f := &fakePusher{block: make(chan struct{})}
	d := &drops{}
	p := publisher(f, d)
	p.MaxWorkers = 2
	book := func(ref string) store.Booking {
		b := subscribed(intents.TnTEvent)
		b.Ref = ref
		return b
	}
	for _, ref := range []string{"A", "B"} {
		if err := p.Publish(book(ref), ev("EV-1")); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Publish(book("C"), ev("EV-1")); err == nil {
		t.Error("a third subscription got a worker past MaxWorkers 2")
	}
	if got := d.list(); len(got) != 1 {
		t.Errorf("drops %v, want the refused push", got)
	}
	close(f.block)
	waitFor(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.queues) == 0
	})
	// With the workers gone, new subscriptions get workers again.
	if err := p.Publish(book("C"), ev("EV-2")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(f.attempts()) == 3 })
	p.Close()
}

type pusherFunc func(ctx context.Context, h envelope.Header, body intents.Body, endpoint string) error

func (f pusherFunc) Push(ctx context.Context, h envelope.Header, body intents.Body, endpoint string) error {
	return f(ctx, h, body, endpoint)
}

func al(id string) *intents.TnTAlertBody {
	return &intents.TnTAlertBody{AlertID: id, Kind: intents.AlertException, Severity: intents.SeverityMedium, Summary: "Customs hold at Rotterdam."}
}
