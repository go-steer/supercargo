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
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sync"
	"time"

	"github.com/go-steer/supercargo/internal/edi"
)

// EDIPartner is a carrier booked over EDIFACT file drops.
type EDIPartner struct {
	ID      string         // its UNB identity
	Out     *edi.Dir       // where instructions go
	In      *edi.Dir       // where its replies arrive
	Zone    *time.Location // agreed for EDI times
	Transit time.Duration  // a leg's planned duration
}

// Gateway is the forwarder's EDI desk (docs/proposals/door-to-door-forwarder.md):
// it sends a leg's transport instruction (IFTMIN) to a carrier without a
// node, takes its acknowledgement (APERAK), and hands its statuses
// (IFTSTA) to the relay, which re-issues them on the order as unsigned.
// EDI never enters ULP: nothing in it is signed, so only what the
// forwarder vouches for, marked as such, reaches its customer. A reply is
// taken only from the identity configured for the carrier, and only for
// a reference the forwarder instructed it with.
type Gateway struct {
	Self     string                 // the forwarder's own UNB identity
	Partners map[string]*EDIPartner // by carrier domain
	// Status handles each status taken (the relay's Unsigned).
	Status func(carrier string, s edi.Status) error
	// AckTimeout is how long Instruct waits for an acknowledgement
	// (default 30s); PollEvery how often it looks (default 100ms).
	AckTimeout time.Duration
	PollEvery  time.Duration
	Now        func() time.Time
	Log        *slog.Logger

	mu         sync.Mutex
	base       int64
	seq        int
	instructed map[string]instructed // by reference
	held       []heldStatus          // statuses whose leg isn't recorded yet
}

// heldStatus is a status taken before its leg was recorded, retried on
// each step until HoldFor has passed.
type heldStatus struct {
	carrier string
	status  edi.Status
	since   time.Time
}

// Bounds on the statuses held for a leg not yet recorded.
const (
	HoldFor = 10 * time.Minute
	MaxHeld = 1000
)

// instructed is an instruction sent: to whom, what, and the answer once
// it came.
type instructed struct {
	carrier     string
	instruction edi.Instruction
	ack         *edi.Ack
}

var (
	// ErrNoAck is an instruction its carrier hasn't acknowledged in time.
	// It stays sent: instructing again waits for the same answer.
	ErrNoAck = errors.New("forwarder: no acknowledgement from the carrier yet")
	// ErrNoPartner is a carrier the gateway has no file drop for.
	ErrNoPartner = errors.New("forwarder: no EDI partner for the carrier")
)

// Instruct sends an instruction to carrier and waits for its
// acknowledgement. Instructing again with the same instruction sends
// nothing new and returns the answer once it's there; a different one
// under the same reference is refused (a change is a new instruction).
func (g *Gateway) Instruct(ctx context.Context, carrier string, i edi.Instruction) (edi.Ack, error) {
	p, ok := g.Partners[carrier]
	if !ok {
		return edi.Ack{}, fmt.Errorf("%w: %s", ErrNoPartner, carrier)
	}
	g.mu.Lock()
	prev, sent := g.instructed[i.Reference]
	var control string
	if !sent {
		if g.instructed == nil {
			g.instructed = map[string]instructed{}
		}
		if g.base == 0 {
			// Control references stay unique across restarts: they count
			// from the clock, not from 1.
			g.base = g.Now().UnixMilli() % 1e13
		}
		g.seq++
		control = fmt.Sprintf("G%013d", (g.base+int64(g.seq))%1e13)
		prev = instructed{carrier: carrier, instruction: i}
		g.instructed[i.Reference] = prev // claimed before it's written: the order's claim keeps out a second Book
	}
	g.mu.Unlock()
	if !sent {
		data, err := edi.WriteIFTMIN(i, edi.Envelope{Sender: g.Self, Recipient: p.ID, Control: control, At: g.Now(), Location: p.Zone})
		if err == nil {
			_, err = p.Out.Put(control, data)
		}
		if err != nil {
			g.mu.Lock()
			delete(g.instructed, i.Reference)
			g.mu.Unlock()
			return edi.Ack{}, fmt.Errorf("forwarder: sending the instruction for %s: %w", i.Reference, err)
		}
		g.Log.Info("instruction sent", "carrier", carrier, "reference", i.Reference)
	}
	if sent && (prev.carrier != carrier || !reflect.DeepEqual(prev.instruction, i)) {
		return edi.Ack{}, fmt.Errorf("forwarder: %s was instructed already, differently (to %s): a change is a new instruction", i.Reference, prev.carrier)
	}
	timeout, every := g.AckTimeout, g.PollEvery
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if every <= 0 {
		every = 100 * time.Millisecond
	}
	deadline := time.Now().Add(timeout)
	for {
		g.Step()
		g.mu.Lock()
		got := g.instructed[i.Reference].ack
		g.mu.Unlock()
		if got != nil {
			return *got, nil
		}
		if time.Now().After(deadline) {
			return edi.Ack{}, fmt.Errorf("%w: %s, from %s", ErrNoAck, i.Reference, carrier)
		}
		select {
		case <-ctx.Done():
			return edi.Ack{}, ctx.Err()
		case <-time.After(every):
		}
	}
}

// Instructed is the instruction sent under ref, and its acknowledgement
// if it came: a retried booking sends the same instruction again (or, if
// the carrier refused it, a new one under a new reference).
func (g *Gateway) Instructed(ref string) (edi.Instruction, *edi.Ack, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	in, ok := g.instructed[ref]
	return in.instruction, in.ack, ok
}

// Step takes the replies that arrived from each partner. A reply that
// isn't the partner's, or names a reference the forwarder didn't instruct
// it with, goes to failed/ for a person.
func (g *Gateway) Step() {
	g.retryHeld()
	for carrier, p := range g.Partners {
		err := p.In.Receive(func(name string, data []byte) error {
			switch {
			case bytes.Contains(data, []byte("APERAK")):
				return g.ack(carrier, p, data)
			case bytes.Contains(data, []byte("IFTSTA")):
				return g.statuses(carrier, p, data)
			}
			return errors.New("forwarder: neither an APERAK nor an IFTSTA")
		})
		if err != nil {
			g.Log.Warn("edi", "carrier", carrier, "error", err)
		}
	}
}

func (g *Gateway) ack(carrier string, p *EDIPartner, data []byte) error {
	a, err := edi.ParseAPERAK(data)
	if err != nil {
		return err
	}
	if a.Sender != p.ID {
		return fmt.Errorf("forwarder: an APERAK from %q, not %s's %q", a.Sender, carrier, p.ID)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	in, ok := g.instructed[a.Reference]
	if !ok || in.carrier != carrier {
		return fmt.Errorf("forwarder: an APERAK for %s, which %s wasn't instructed with", a.Reference, carrier)
	}
	if in.ack == nil {
		in.ack = &a
		g.instructed[a.Reference] = in
	}
	return nil
}

func (g *Gateway) statuses(carrier string, p *EDIPartner, data []byte) error {
	sts, err := edi.ParseIFTSTAIn(data, p.Zone)
	if err != nil {
		return err
	}
	g.mu.Lock()
	for _, s := range sts {
		in, ok := g.instructed[s.Reference]
		if s.Sender != p.ID || !ok || in.carrier != carrier || in.ack == nil || !in.ack.Accepted {
			g.mu.Unlock()
			return fmt.Errorf("forwarder: a status from %q for %s, which %s hasn't taken", s.Sender, s.Reference, carrier)
		}
	}
	g.mu.Unlock()
	var errs []error
	for _, s := range sts {
		if g.Status == nil {
			continue
		}
		err := g.Status(carrier, s)
		if errors.Is(err, ErrSkipped) {
			g.Log.Warn("edi status skipped", "carrier", carrier, "reference", s.Reference, "error", err)
			continue
		}
		if errors.Is(err, ErrNoLeg) {
			// Acknowledged, but its leg not recorded yet: held, not lost.
			g.mu.Lock()
			if len(g.held) < MaxHeld {
				g.held = append(g.held, heldStatus{carrier, s, g.Now()})
				err = nil
			}
			g.mu.Unlock()
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// retryHeld hands on the held statuses whose leg is recorded now, and
// drops those held past HoldFor.
func (g *Gateway) retryHeld() {
	g.mu.Lock()
	held := g.held
	g.held = nil
	g.mu.Unlock()
	var keep []heldStatus
	for _, h := range held {
		err := g.Status(h.carrier, h.status)
		switch {
		case errors.Is(err, ErrNoLeg) && g.Now().Sub(h.since) < HoldFor:
			keep = append(keep, h)
		case err != nil:
			g.Log.Warn("edi status dropped", "carrier", h.carrier, "reference", h.status.Reference, "error", err)
		}
	}
	g.mu.Lock()
	g.held = append(keep, g.held...)
	g.mu.Unlock()
}

// Run takes replies every tick until ctx is done.
func (g *Gateway) Run(ctx context.Context, tick time.Duration) {
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			g.Step()
		}
	}
}
