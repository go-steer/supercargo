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
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
	"github.com/go-steer/supercargo/pkg/transport/rest"
)

// Pusher signs and posts a push to a registered endpoint. *node.Node
// implements it.
type Pusher interface {
	Push(ctx context.Context, h envelope.Header, body intents.Body, endpoint string) error
}

// Publisher records events and alerts in the operator's log and pushes
// them to the booking's subscriber (docs/DESIGN.md, A.8).
//
// Each subscription with pushes pending has its own queue and worker, so a
// subscriber sees a booking's events in the order they were published, and
// one slow subscriber can't hold up another's. A worker exits once its queue
// is empty, and the number of active workers is capped: bookings are free to
// make, so idle subscriptions must cost nothing.
//
// A failed delivery is retried with backoff only when retrying can help.
// Each attempt is a newly signed message (a fresh messageId and sentAt: a
// retried copy of the old one would be a replay, or stale), and the
// subscriber records each event once.
type Publisher struct {
	Pusher Pusher
	Domain string // this operator
	Log    *store.EventLog
	Now    func() time.Time
	// Backoff is the wait before each retry; its length bounds the retries.
	// Nil means 1s, 2s, 4s, 8s, 16s, 30s, 30s (eight attempts).
	Backoff []time.Duration
	// QueueLen bounds each subscription's queue (default 256).
	QueueLen int
	// MaxWorkers bounds subscriptions with pushes in flight (default 1024).
	MaxWorkers int
	// OnDrop learns about every push given up on: refused, out of retries,
	// over a limit, or abandoned at Close. Optional; never called with the
	// publisher's lock held, but may run on a worker, so it must not call
	// Close. A dropped push stays in the log and its ID stays used: the
	// subscriber catches up by asking (ulp.tnt.inquiry), not by a republish.
	OnDrop func(booking, id string, err error)
	// OnRetry learns about every push about to be tried again, for the
	// node's metrics. Optional; the same rules as OnDrop.
	OnRetry func(booking, id string)
	// Bookings, if set, is where a booking's cancellation is read: a
	// booking cancelled there is refused (ErrCancelled), under the lock
	// that records, so nothing is recorded on it once it's cancelled.
	Bookings *store.Bookings

	once   sync.Once
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.Mutex
	queues map[string]chan push
	closed bool
}

type push struct {
	booking store.Booking
	body    intents.Body // a private copy
	id      string
}

var defaultBackoff = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}

var (
	// ErrCancelled reports a Publish on a cancelled booking.
	ErrCancelled = errors.New("operator: the booking was cancelled")
	// ErrClosed reports a Publish after Close.
	ErrClosed = errors.New("operator: publisher closed")
	// ErrDuplicate reports an event or alert ID already published for the
	// booking. Each update needs its own ID: the subscriber keeps the
	// first body it sees under an ID.
	ErrDuplicate = errors.New("operator: ID already published for this booking")
	// ErrNotQueued reports a push recorded in the log but not queued for
	// delivery (a queue or the worker cap was full); OnDrop has been told.
	ErrNotQueued = errors.New("recorded, not queued")
)

func (p *Publisher) init() {
	p.once.Do(func() {
		p.ctx, p.cancel = context.WithCancel(context.Background())
		p.queues = map[string]chan push{}
		if p.Backoff == nil {
			p.Backoff = defaultBackoff
		}
		if p.QueueLen <= 0 {
			p.QueueLen = 256
		}
		if p.MaxWorkers <= 0 {
			p.MaxWorkers = 1024
		}
		if p.Now == nil {
			p.Now = time.Now
		}
	})
}

// Publish records body (a tnt.event or tnt.alert) against booking b and,
// if b's subscription asks for that intent, queues it for its subscriber.
func (p *Publisher) Publish(b store.Booking, body intents.Body) error {
	p.init()
	if i := body.Intent(); i != intents.TnTEvent && i != intents.TnTAlert {
		return fmt.Errorf("operator: %s is not something to publish", i)
	}
	if err := body.Validate(); err != nil {
		return fmt.Errorf("operator: invalid %s: %w", body.Intent(), err)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	// The queue keeps its own copy: the caller may reuse its struct, and
	// what is pushed must be what was logged.
	own, err := copyBody(body, raw)
	if err != nil {
		return err
	}
	id := pushID(body)

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return ErrClosed
	}
	if p.Bookings != nil {
		if cur, err := p.Bookings.Get(b.Operator, b.Ref); err == nil && cur.Cancelled() {
			p.mu.Unlock()
			return fmt.Errorf("%w: %s", ErrCancelled, b.Ref)
		}
	}
	added, err := p.Log.Add(b.Operator, b.Ref, store.Record{ID: id, Intent: body.Intent(), Body: raw, At: p.Now()})
	switch {
	case err != nil:
		p.mu.Unlock()
		return fmt.Errorf("operator: recording %s: %w", id, err)
	case !added:
		p.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrDuplicate, id)
	}
	sub := b.Subscription()
	if sub == nil || !slices.Contains(sub.Intents, body.Intent()) {
		p.mu.Unlock()
		return nil // recorded; nobody asked for it
	}
	key := b.Shipper + "/" + b.Ref
	q, ok := p.queues[key]
	if !ok {
		if len(p.queues) >= p.MaxWorkers {
			p.mu.Unlock()
			return p.dropped(b.Ref, id, errors.New("too many subscriptions with pushes in flight"))
		}
		q = make(chan push, p.QueueLen)
		p.queues[key] = q
		p.wg.Add(1)
		go p.work(key, q)
	}
	select {
	case q <- push{booking: b, body: own, id: id}:
		p.mu.Unlock()
		return nil
	default:
		p.mu.Unlock()
		return p.dropped(b.Ref, id, errors.New("subscription queue full"))
	}
}

// Exclusive runs fn with nothing published meanwhile: a cancellation
// checks that nothing has moved and stores itself in one step. fn must
// not publish.
func (p *Publisher) Exclusive(fn func() error) error {
	p.init()
	p.mu.Lock()
	defer p.mu.Unlock()
	return fn()
}

// dropped reports a push not queued and returns the error for Publish.
func (p *Publisher) dropped(booking, id string, err error) error {
	p.drop(booking, id, err)
	return fmt.Errorf("operator: %s: %w: %w", id, ErrNotQueued, err)
}

// Close stops the workers and waits for them. Pushes still queued or
// waiting to retry are abandoned and reported to OnDrop.
func (p *Publisher) Close() {
	p.init()
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	p.cancel()
	p.wg.Wait()
}

func (p *Publisher) work(key string, q chan push) {
	defer p.wg.Done()
	for {
		select {
		case <-p.ctx.Done():
			p.abandon(q)
			return
		case item := <-q:
			p.deliver(item)
		default:
			// Idle: leave, unless something arrived meanwhile. Sends happen
			// under p.mu, so nothing can slip in between the check and the
			// delete, and the next Publish starts a fresh worker.
			p.mu.Lock()
			if len(q) == 0 {
				delete(p.queues, key)
				p.mu.Unlock()
				return
			}
			p.mu.Unlock()
		}
	}
}

// abandon reports what is left in q at Close.
func (p *Publisher) abandon(q chan push) {
	for {
		select {
		case item := <-q:
			p.drop(item.booking.Ref, item.id, ErrClosed)
		default:
			return
		}
	}
}

func (p *Publisher) deliver(item push) {
	b := item.booking
	var err error
	for attempt := 0; ; attempt++ {
		h := envelope.NewHeader(item.body.Intent(), p.Domain, b.Shipper, p.Now())
		ref := b.Ref
		h.BookingRef = &ref
		err = p.Pusher.Push(p.ctx, h, item.body, b.Subscription().Endpoint)
		if err == nil || !retryable(err) || attempt >= len(p.Backoff) {
			break
		}
		select {
		case <-p.ctx.Done():
			p.drop(b.Ref, item.id, ErrClosed)
			return
		case <-time.After(p.Backoff[attempt]):
		}
		if p.OnRetry != nil {
			p.OnRetry(b.Ref, item.id)
		}
	}
	if err != nil {
		p.drop(b.Ref, item.id, err)
	}
}

// Pending is the number of pushes queued and not yet being delivered.
func (p *Publisher) Pending() int {
	p.init()
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, q := range p.queues {
		n += len(q)
	}
	return n
}

func (p *Publisher) drop(booking, id string, err error) {
	if p.OnDrop != nil {
		p.OnDrop(booking, id, err)
	}
}

// retryable reports whether a failed push is worth another attempt. Only
// failures another attempt can fix qualify: the HTTP exchange itself
// failing, a bare 5xx/408/429, or the subscriber's retryable ulp.error.
// Everything else, from an endpoint the guard refuses to a sealing error,
// would fail the same way again.
func retryable(err error) bool {
	if errors.Is(err, rest.ErrRefused) {
		return false
	}
	var re *node.RemoteError
	if errors.As(err, &re) {
		return re.Retryable
	}
	var se *rest.StatusError
	if errors.As(err, &se) {
		return se.Retryable()
	}
	var te *rest.TransportError
	return errors.As(err, &te) && !errors.Is(err, context.Canceled)
}

func pushID(body intents.Body) string {
	switch b := body.(type) {
	case *intents.TnTEventBody:
		return b.EventID
	case *intents.TnTAlertBody:
		return b.AlertID
	}
	return ""
}

// copyBody decodes raw into a fresh value of body's type.
func copyBody(body intents.Body, raw []byte) (intents.Body, error) {
	var out intents.Body
	switch body.(type) {
	case *intents.TnTEventBody:
		out = &intents.TnTEventBody{}
	case *intents.TnTAlertBody:
		out = &intents.TnTAlertBody{}
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return nil, err
	}
	return out, nil
}
