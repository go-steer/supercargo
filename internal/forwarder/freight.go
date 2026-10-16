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
	"math"
	"strings"
	"sync"
	"time"

	"github.com/go-steer/supercargo/internal/antt"
	"github.com/go-steer/supercargo/internal/backend"
	"github.com/go-steer/supercargo/internal/config"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/intents"
)

// Freight floors (docs/DESIGN.md, freight floors): a Brazilian road leg
// may not be contracted below the ANTT's minimum freight. A lane leg with
// a freight section carries the forwarder's rate; before the leg is
// booked, its route and floor come from the FreightFloors port (QualP),
// and a rate below the floor, or a floor that can't be had, stops the
// booking before any leg is sent. A person may set an order's rate for a
// leg (the dialog); a model only drafts it. The rate stays here: it is
// never in an envelope.

// Freight statuses.
const (
	FreightOK        = "ok"        // the rate is at or above the floor
	FreightBelow     = "below"     // the rate is below the floor
	FreightUnchecked = "unchecked" // no floor could be had
)

// FreightCurrency is the rates' and floors' currency.
const FreightCurrency = "BRL"

var (
	// ErrBelowFloor is a leg whose rate is below the ANTT's floor.
	ErrBelowFloor = errors.New("forwarder: a leg's freight is below the ANTT minimum")
	// ErrFloorUnchecked is a leg whose floor couldn't be had: it isn't
	// booked unchecked.
	ErrFloorUnchecked = errors.New("forwarder: a leg's freight floor couldn't be checked")
	// ErrNoFreight is a rate set on a leg that has no freight floor (or
	// is booked already), or a rate out of range.
	ErrNoFreight = errors.New("forwarder: no freight rate to set on this leg")
)

// LegFreight is a road leg's freight against its floor, as the plan
// shows it.
type LegFreight struct {
	Rate     float64 `json:"rate"`
	Currency string  `json:"currency"`
	// LaneRate is the routing guide's rate, when the order's differs (a
	// person set it).
	LaneRate float64        `json:"laneRate,omitempty"`
	Axles    int            `json:"axles"`
	LoadType string         `json:"loadType"`
	Table    string         `json:"table"`
	Route    []string       `json:"route"`
	Floor    *backend.Floor `json:"floor,omitempty"`
	Status   string         `json:"status"`
	// Shortfall is how far below the floor the rate is.
	Shortfall float64 `json:"shortfall,omitempty"`
	Error     string  `json:"error,omitempty"`
}

// floorTTL is how long a floor is kept: the floor changes with a new
// resolution, the tolls with the concessions' readjustments.
const (
	floorTTL  = time.Hour
	failTTL   = time.Minute
	maxFloors = 1000
)

// held is a floor the check may rely on: a positive amount for a
// positive distance, never below the ANTT's method applied to that
// distance here (a port's adapter is trusted for the route, not to lower
// the floor).
func held(f *backend.Floor, r backend.FloorRequest) (*backend.Floor, error) {
	if f == nil || !(f.Freight > 0) || !(f.DistanceKm > 0) {
		return nil, errors.New("the floor source gave no usable floor")
	}
	own, _, err := antt.Floor(r.Table, r.LoadType, r.Axles, f.DistanceKm)
	if err != nil {
		return nil, fmt.Errorf("the floor can't be checked here: %w", err)
	}
	if own > f.Freight {
		g := *f
		g.Freight = own
		return &g, nil
	}
	return f, nil
}

// floorCache holds floors by request, so a plan's every view doesn't call
// (and pay) QualP.
type floorCache struct {
	mu sync.Mutex
	m  map[string]floorEntry
}

type floorEntry struct {
	floor *backend.Floor
	err   error
	at    time.Time
}

// floor is the leg's floor: kept, or from o.Floors.
func (o *Orchestrator) floor(ctx context.Context, r backend.FloorRequest) (*backend.Floor, error) {
	if o.Floors == nil {
		return nil, errors.New("no freight floor source")
	}
	key := fmt.Sprintf("%q|%d|%s|%s", r.Locations, r.Axles, r.LoadType, r.Table)
	now := o.Now()
	c := &o.floors
	c.mu.Lock()
	if e, ok := c.m[key]; ok && (e.err == nil && now.Sub(e.at) < floorTTL || e.err != nil && now.Sub(e.at) < failTTL) {
		c.mu.Unlock()
		return e.floor, e.err
	}
	c.mu.Unlock()
	f, err := o.Floors.Floor(ctx, r)
	if errors.Is(err, context.Canceled) {
		return nil, err // the caller left: nothing learned
	}
	if err == nil {
		f, err = held(f, r)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]floorEntry{}
	}
	if len(c.m) >= maxFloors {
		// Expired first; still full, the oldest.
		var oldest string
		for k, e := range c.m {
			if now.Sub(e.at) >= floorTTL {
				delete(c.m, k)
			} else if oldest == "" || e.at.Before(c.m[oldest].at) {
				oldest = k
			}
		}
		if len(c.m) >= maxFloors {
			delete(c.m, oldest)
		}
	}
	c.m[key] = floorEntry{floor: f, err: err, at: now}
	return f, err
}

// checkFreight fills in each unbooked step's freight: its rate (the
// order's, else the lane's) against its floor.
func (o *Orchestrator) checkFreight(ctx context.Context, b store.Booking, steps []Step) {
	lane := o.lane(b)
	if lane == nil {
		return
	}
	req := b.Order.Request
	for i := range steps {
		if i >= len(lane.Legs) || lane.Legs[i].Freight == nil {
			continue
		}
		lf := lane.Legs[i].Freight
		f := &LegFreight{Rate: lf.Rate, Currency: FreightCurrency, Axles: lf.Axles, LoadType: lf.LoadType, Table: lf.Table}
		if r, ok := b.Order.Rates[i+1]; ok && r != lf.Rate {
			f.Rate, f.LaneRate = r, lf.Rate
		}
		from, to := lf.From, lf.To
		if from == "" {
			from = steps[i].From
			if i == 0 {
				from = town(req.PlaceOfReceipt)
			}
		}
		if to == "" {
			to = steps[i].To
			if i == len(steps)-1 {
				to = town(req.PlaceOfDelivery)
			}
		}
		f.Route = []string{from, to}
		steps[i].Freight = f
		if steps[i].Booked != "" {
			f.Status = FreightOK // booked already: checked then
			continue
		}
		floor, err := o.floor(ctx, backend.FloorRequest{Locations: f.Route, Axles: f.Axles, LoadType: f.LoadType, Table: f.Table})
		switch {
		case err != nil:
			f.Status, f.Error = FreightUnchecked, err.Error()
		case !(f.Rate >= floor.Freight):
			f.Floor, f.Status, f.Shortfall = floor, FreightBelow, math.Round((floor.Freight-f.Rate)*100)/100
		default:
			f.Floor, f.Status = floor, FreightOK
		}
	}
}

// freightHolds is nil if every unbooked leg's freight may be booked: at
// or above its floor, checked.
func freightHolds(steps []Step) error {
	for i, s := range steps {
		f := s.Freight
		if f == nil || s.Booked != "" {
			continue
		}
		switch f.Status {
		case FreightBelow:
			return fmt.Errorf("%w: leg %d (%s): the rate R$ %.2f is R$ %.2f below the floor of R$ %.2f (%s); change the leg's rate first",
				ErrBelowFloor, i+1, s.Name, f.Rate, f.Shortfall, f.Floor.Freight, f.Floor.Resolution)
		case FreightOK:
		default:
			return fmt.Errorf("%w: leg %d (%s): %s", ErrFloorUnchecked, i+1, s.Name, f.Error)
		}
	}
	return nil
}

// lane is the lane an order is planned on, or nil.
func (o *Orchestrator) lane(b store.Booking) *config.Lane {
	req := b.Order.Request
	from, to := req.PlaceOfReceipt.UNLocationCode, req.PlaceOfDelivery.UNLocationCode
	for i, l := range o.Lanes {
		if l.From != from || l.To != to {
			continue
		}
		if req.PlaceOfReceipt.Type == intents.PlaceDoor && l.Legs[0].Mode != intents.ModeRoad ||
			req.PlaceOfDelivery.Type == intents.PlaceDoor && l.Legs[len(l.Legs)-1].Mode != intents.ModeRoad {
			continue
		}
		return &o.Lanes[i]
	}
	return nil
}

// town names a place for a route: its town and state (or country), else
// its UN/LOCODE.
func town(p intents.Place) string {
	if a := p.Address; a != nil && strings.TrimSpace(a.Locality) != "" {
		if a.Region != "" {
			return a.Locality + ", " + a.Region
		}
		return a.Locality + ", " + a.Country
	}
	return p.UNLocationCode
}

// PlanChecked is the order's plan with each unbooked road leg's freight
// checked against its floor.
func (o *Orchestrator) PlanChecked(ctx context.Context, ref string) ([]Step, error) {
	b, err := o.Orders.Bookings.Get(o.Orders.Node.Domain(), ref)
	if err != nil || b.Order == nil {
		return nil, fmt.Errorf("%w: %s", ErrUnknownOrder, ref)
	}
	steps, err := o.plan(b)
	if err != nil {
		return nil, err
	}
	if b.Order.Confirmation.Status == intents.StatusPending {
		o.checkFreight(ctx, b, steps)
	}
	return steps, nil
}

// SetRate sets a pending order's freight rate for one leg (1-based), not
// booked yet, that has a freight floor: the step a person confirms in a
// dialog. It may be below the floor (booking then refuses it); the plan,
// checked again, is the answer.
func (o *Orchestrator) SetRate(ctx context.Context, ref string, leg int, rate float64) ([]Step, error) {
	rate = math.Round(rate*100) / 100
	if !(rate > 0 && rate <= config.MaxRate) {
		return nil, fmt.Errorf("%w: a rate is over 0 and at most %d reais", ErrNoFreight, config.MaxRate)
	}
	release, err := o.Orders.claim(ref)
	if err != nil {
		return nil, err
	}
	defer release()
	self := o.Orders.Node.Domain()
	b, err := o.Orders.Bookings.Get(self, ref)
	if err != nil || b.Order == nil {
		return nil, fmt.Errorf("%w: %s", ErrUnknownOrder, ref)
	}
	if b.Order.Confirmation.Status != intents.StatusPending {
		return nil, fmt.Errorf("%w: %s is %s", ErrAnswered, ref, b.Order.Confirmation.Status)
	}
	lane := o.lane(b)
	if lane == nil {
		return nil, fmt.Errorf("%w: %s", ErrNoLane, ref)
	}
	switch {
	case leg < 1 || leg > len(lane.Legs) || lane.Legs[leg-1].Freight == nil:
		return nil, fmt.Errorf("%w: leg %d of %s has no freight floor", ErrNoFreight, leg, ref)
	case leg <= len(b.Order.Legs):
		return nil, fmt.Errorf("%w: leg %d of %s is booked already", ErrNoFreight, leg, ref)
	}
	if _, err := o.Orders.Bookings.Update(self, ref, func(b *store.Booking) error {
		if b.Order == nil || b.Order.Confirmation.Status != intents.StatusPending || leg <= len(b.Order.Legs) {
			return errors.New("forwarder: the order changed meanwhile")
		}
		if b.Order.Rates == nil {
			b.Order.Rates = map[int]float64{}
		}
		b.Order.Rates[leg] = rate
		return nil
	}); err != nil {
		return nil, err
	}
	b, _ = o.Orders.Bookings.Get(self, ref)
	steps, err := o.plan(b)
	if err != nil {
		return nil, err
	}
	o.checkFreight(ctx, b, steps)
	return steps, nil
}
