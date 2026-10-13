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

// Package mockcarrier is the demo's stand-in for carriers without a ULP
// node (docs/proposals/backend-integration.md, "mock backends"): what a
// company would need to connect, shown working. EDIFACT is a road carrier
// that takes transport instructions and reports status as EDIFACT files,
// as a small European trucker does.
package mockcarrier

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/go-steer/supercargo/internal/edi"
)

// EDIFACT is a road carrier on a file drop. For each instruction (IFTMIN)
// from its partner it answers an acknowledgement (APERAK, accepted, with
// an e-CMR number), then reports the trip as statuses (IFTSTA) on an
// accelerated clock: picked up at the pickup window's start, departed,
// gated in at the delivery place, delivered. It sends the same
// acknowledgement again for an instruction it already took.
type EDIFACT struct {
	ID      string   // its own UNB identity, e.g. polder-haulage.example
	Partner string   // the forwarder's, whose instructions it takes
	In      *edi.Dir // where instructions arrive
	Out     *edi.Dir // where its replies go
	// ECMRPrefix numbers its e-CMRs, e.g. "ECMR-NL-2026".
	ECMRPrefix string
	// Zone is the zone agreed for EDI times (UTC if nil).
	Zone *time.Location
	// HoursPerSecond is the trips' clock speed (default 1).
	HoursPerSecond float64
	// Transit is how long a trip takes when the instruction gives no
	// delivery window (default 3h).
	Transit time.Duration
	Now     func() time.Time
	Log     *slog.Logger

	seq   int
	acked map[string]edi.Ack // by instruction reference
	trips []*trip
}

// trip is one instruction's statuses still to send, on its own clock.
type trip struct {
	realStart, simStart time.Time
	statuses            []edi.Status
}

// Step takes the instructions that arrived and sends the replies and
// statuses due. Run calls it on a ticker; tests call it directly.
func (c *EDIFACT) Step() error {
	if c.acked == nil {
		c.acked = map[string]edi.Ack{}
	}
	err := c.In.Receive(func(name string, data []byte) error {
		i, sender, err := edi.ParseIFTMIN(data, c.Zone)
		if err != nil {
			return err // to failed/: a person looks at it
		}
		if sender != c.Partner {
			return fmt.Errorf("mockcarrier: an instruction from %q, not %q", sender, c.Partner)
		}
		if ack, ok := c.acked[i.Reference]; ok {
			return c.send("APERAK", func(env edi.Envelope) ([]byte, error) { return edi.WriteAPERAK(ack, env) })
		}
		c.seq++
		ack := edi.Ack{Reference: i.Reference, CarrierRef: fmt.Sprintf("PH%04d", 4470+c.seq), Accepted: true}
		if c.ECMRPrefix != "" {
			ack.TransportDocuments = []string{fmt.Sprintf("%s-%06d", c.ECMRPrefix, 4470+c.seq)}
		}
		if err := c.send("APERAK", func(env edi.Envelope) ([]byte, error) { return edi.WriteAPERAK(ack, env) }); err != nil {
			return err
		}
		c.acked[i.Reference] = ack
		c.trips = append(c.trips, c.plan(i, ack))
		c.Log.Info("instruction accepted", "reference", i.Reference, "carrierRef", ack.CarrierRef)
		return nil
	})
	if err != nil {
		c.Log.Warn("instructions", "error", err)
	}
	return c.report()
}

// plan schedules a trip's statuses.
func (c *EDIFACT) plan(i edi.Instruction, ack edi.Ack) *trip {
	pickup := i.Pickup.Earliest
	if pickup.IsZero() {
		pickup = c.Now().UTC().Truncate(time.Hour).Add(time.Hour)
	}
	transit := c.Transit
	if transit <= 0 {
		transit = 3 * time.Hour
	}
	arrival := pickup.Add(transit)
	if l := i.Delivery.Latest; !l.IsZero() && l.Before(arrival) {
		arrival = l
	}
	doc := ""
	if len(ack.TransportDocuments) > 0 {
		doc = ack.TransportDocuments[0]
	}
	st := func(code string, at time.Time, s edi.Stop) edi.Status {
		return edi.Status{Reference: i.Reference, CarrierRef: ack.CarrierRef, Code: code, Time: at, UNLocode: s.UNLocode, Facility: s.Name, TransportDocument: doc}
	}
	return &trip{realStart: c.Now(), simStart: pickup.Add(-time.Hour), statuses: []edi.Status{
		st("12", pickup, i.Pickup),                        // picked up
		st("13", pickup.Add(15*time.Minute), i.Pickup),    // departed
		st("54", arrival, i.Delivery),                     // gated in
		st("21", arrival.Add(30*time.Minute), i.Delivery), // delivered
	}}
}

// report sends each trip's statuses that are due, one IFTSTA each.
func (c *EDIFACT) report() error {
	speed := c.HoursPerSecond
	if speed <= 0 {
		speed = 1
	}
	now := c.Now()
	var left []*trip
	for _, t := range c.trips {
		sim := t.simStart.Add(time.Duration(float64(now.Sub(t.realStart)) * speed * 3600))
		sort.SliceStable(t.statuses, func(i, j int) bool { return t.statuses[i].Time.Before(t.statuses[j].Time) })
		for len(t.statuses) > 0 && !t.statuses[0].Time.After(sim) {
			s := t.statuses[0]
			if err := c.send("IFTSTA", func(env edi.Envelope) ([]byte, error) { return edi.WriteIFTSTA([]edi.Status{s}, env) }); err != nil {
				return err
			}
			t.statuses = t.statuses[1:]
			c.Log.Info("status sent", "reference", s.Reference, "code", s.Code)
		}
		if len(t.statuses) > 0 {
			left = append(left, t)
		}
	}
	c.trips = left
	return nil
}

// send writes one interchange to the out drop.
func (c *EDIFACT) send(kind string, write func(edi.Envelope) ([]byte, error)) error {
	c.seq++
	env := edi.Envelope{Sender: c.ID, Recipient: c.Partner, Control: fmt.Sprintf("PH%06d", c.seq), At: c.Now(), Location: c.Zone}
	data, err := write(env)
	if err != nil {
		return fmt.Errorf("mockcarrier: writing %s: %w", kind, err)
	}
	_, err = c.Out.Put(env.Control, data)
	return err
}

// Run steps every tick until ctx is done.
func (c *EDIFACT) Run(ctx context.Context, tick time.Duration) error {
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := c.Step(); err != nil {
				c.Log.Warn("step", "error", err)
			}
		}
	}
}
