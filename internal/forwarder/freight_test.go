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
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/backend"
	"github.com/go-steer/supercargo/internal/config"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/intents"
)

// floors is a FreightFloors: Santos → Cajamar's floor, or err; it counts
// its calls and keeps the last request.
type floors struct {
	err   error
	calls int
	got   backend.FloorRequest
}

func (f *floors) Floor(_ context.Context, r backend.FloorRequest) (*backend.Floor, error) {
	f.calls++
	f.got = r
	if f.err != nil {
		return nil, f.err
	}
	return &backend.Floor{DistanceKm: 117.6, Duration: 2 * time.Hour, TollTotal: 242, Freight: 1140.01, LoadUnload: 497.66,
		Tolls: []backend.Toll{{Name: "Riacho Grande", Amount: 179.5}}, Resolution: "Resolução ANTT nº 6.034", Source: "QualP (simulated)"}, nil
}

// freightLane is portToDoor with Serra's truck at a rate under the floor.
func freightLane(rate float64) config.Lane {
	l := portToDoor
	l.Legs = slices.Clone(l.Legs)
	l.Legs[1].Freight = &config.LaneFreight{Rate: rate, Axles: 5, LoadType: "conteineirizada", Table: "A", From: "Santos, SP"}
	return l
}

func freightOrder(t *testing.T, legs *carriers, f *floors, rate float64) *Orchestrator {
	t.Helper()
	orch, _ := orchestrated(t, legs, freightLane(rate))
	orch.Floors = f
	o := doorOrder()
	o.PlaceOfDelivery.Address.Region = "SP"
	if _, err := orch.Orders.Handle(context.Background(), orderFrom(cymbal, o)); err != nil {
		t.Fatal(err)
	}
	return orch
}

// TestFreightBelowFloor: a truck leg priced below the ANTT floor shows so
// on the plan, and booking stops before any leg is sent; a rate a person
// sets at or above it books.
func TestFreightBelowFloor(t *testing.T) {
	ctx := context.Background()
	legs, f := &carriers{}, &floors{}
	orch := freightOrder(t, legs, f, 1050)
	steps, err := orch.PlanChecked(ctx, "MF-0007")
	if err != nil {
		t.Fatal(err)
	}
	fr := steps[1].Freight
	if steps[0].Freight != nil || fr == nil || fr.Status != FreightBelow || fr.Shortfall != 90.01 || fr.Floor.Freight != 1140.01 ||
		!slices.Equal(fr.Route, []string{"Santos, SP", "Cajamar, SP"}) || fr.Currency != "BRL" {
		t.Fatalf("freight %+v", fr)
	}
	if !slices.Equal(f.got.Locations, []string{"Santos, SP", "Cajamar, SP"}) || f.got.Axles != 5 || f.got.LoadType != "conteineirizada" || f.got.Table != "A" {
		t.Errorf("asked %+v", f.got)
	}
	if _, err := orch.Book(ctx, "MF-0007"); !errors.Is(err, ErrBelowFloor) || !strings.Contains(err.Error(), "R$ 90.01 below") {
		t.Fatalf("booked below the floor: %v", err)
	}
	if len(legs.ocean)+len(legs.road) != 0 {
		t.Fatalf("legs sent before the floor held: %v %v", legs.ocean, legs.road)
	}
	steps, err = orch.SetRate(ctx, "MF-0007", 2, 1180)
	if err != nil || steps[1].Freight.Status != FreightOK || steps[1].Freight.Rate != 1180 || steps[1].Freight.LaneRate != 1050 {
		t.Fatalf("rate set: %+v, %v", steps[1].Freight, err)
	}
	if _, err := orch.Book(ctx, "MF-0007"); err != nil {
		t.Fatal(err)
	}
	b, _ := orch.Orders.Bookings.Get(meridian, "MF-0007")
	if b.Order.Rates[2] != 1180 || len(b.Order.Legs) != 2 {
		t.Errorf("order %+v", b.Order)
	}
	// The rate is ours: never in the truck's booking.
	raw, _ := json.Marshal(legs.got)
	if strings.Contains(string(raw), "1180") || strings.Contains(string(raw), "1050") {
		t.Errorf("the rate went to the carrier: %s", raw)
	}
	// Answered: the plan isn't checked again (no call paid for it).
	calls := f.calls
	if steps, _ := orch.PlanChecked(ctx, "MF-0007"); steps[1].Freight != nil || f.calls != calls {
		t.Errorf("an answered order checked again: %+v", steps[1].Freight)
	}
}

// badFloors answers a floor no check may rely on.
type badFloors struct{ floor *backend.Floor }

func (b badFloors) Floor(context.Context, backend.FloorRequest) (*backend.Floor, error) {
	return b.floor, nil
}

// TestFloorHeld: a source's floor that is missing, zero or not a number
// is unchecked, never passed; one under the ANTT's method for its own
// distance is raised to it.
func TestFloorHeld(t *testing.T) {
	ctx := context.Background()
	for name, fl := range map[string]*backend.Floor{
		"nil":           nil,
		"zero":          {DistanceKm: 117.6},
		"not a number":  {DistanceKm: 117.6, Freight: math.NaN()},
		"zero distance": {Freight: 1140.01},
	} {
		legs := &carriers{}
		orch, _ := orchestrated(t, legs, freightLane(50))
		orch.Floors = badFloors{fl}
		if _, err := orch.Orders.Handle(ctx, orderFrom(cymbal, doorOrder())); err != nil {
			t.Fatal(err)
		}
		if _, err := orch.Book(ctx, "MF-0007"); !errors.Is(err, ErrFloorUnchecked) || len(legs.ocean)+len(legs.road) != 0 {
			t.Errorf("%s: %v, legs %v %v", name, err, legs.ocean, legs.road)
		}
	}
	// Under the method (R$ 1.140,01 for 117.6 km): the method's floor.
	orch, _ := orchestrated(t, &carriers{}, freightLane(1050))
	orch.Floors = badFloors{&backend.Floor{DistanceKm: 117.6, Freight: 100, Source: "low"}}
	if _, err := orch.Orders.Handle(ctx, orderFrom(cymbal, doorOrder())); err != nil {
		t.Fatal(err)
	}
	steps, _ := orch.PlanChecked(ctx, "MF-0007")
	if fr := steps[1].Freight; fr.Status != FreightBelow || fr.Floor.Freight != 1140.01 {
		t.Errorf("a lowered floor: %+v", fr)
	}
}

// TestRateWhileBooking: a rate set while the order is being booked is
// refused, as a second booking is.
func TestRateWhileBooking(t *testing.T) {
	orch := freightOrder(t, &carriers{}, &floors{}, 1050)
	release, err := orch.Orders.claim("MF-0007")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := orch.SetRate(context.Background(), "MF-0007", 2, 1200); !errors.Is(err, ErrAnswered) {
		t.Errorf("during a booking: %v", err)
	}
	if _, err := orch.SetRate(context.Background(), "MF-0007", 2, 0.004); !errors.Is(err, ErrNoFreight) {
		t.Errorf("a rate rounding to zero: %v", err)
	}
}

// TestResumeBelowFloor: a resumed booking (the ship booked before) still
// stops at a truck below its floor, sending nothing more.
func TestResumeBelowFloor(t *testing.T) {
	ctx := context.Background()
	legs := &carriers{refuseRoad: true}
	orch := freightOrder(t, legs, &floors{}, 1200)
	if _, err := orch.Book(ctx, "MF-0007"); !errors.Is(err, ErrLegRefused) || len(legs.ocean) != 1 {
		t.Fatalf("first booking: %v, %v", err, legs.ocean)
	}
	orch.Lanes[0].Legs[1].Freight.Rate = 1000 // the routing guide lowered meanwhile
	legs.refuseRoad = false
	if _, err := orch.Book(ctx, "MF-0007"); !errors.Is(err, ErrBelowFloor) || len(legs.ocean) != 1 || len(legs.road) != 0 {
		t.Errorf("resumed below the floor: %v, %v %v", err, legs.ocean, legs.road)
	}
}

// TestFreightFailsClosed: a floor that can't be had stops the booking,
// and an error is kept only briefly.
func TestFreightFailsClosed(t *testing.T) {
	ctx := context.Background()
	legs, f := &carriers{}, &floors{err: errors.New("qualp: HTTP 503")}
	orch := freightOrder(t, legs, f, 5000)
	if _, err := orch.Book(ctx, "MF-0007"); !errors.Is(err, ErrFloorUnchecked) {
		t.Fatalf("booked unchecked: %v", err)
	}
	if len(legs.ocean)+len(legs.road) != 0 {
		t.Fatal("legs sent unchecked")
	}
	steps, _ := orch.PlanChecked(ctx, "MF-0007")
	if fr := steps[1].Freight; fr.Status != FreightUnchecked || fr.Error == "" || f.calls != 1 {
		t.Fatalf("freight %+v after %d calls", fr, f.calls)
	}
	// After failTTL, asked again, and now it holds (5000 is above).
	now := orch.Now()
	orch.Now = func() time.Time { return now.Add(failTTL + time.Second) }
	f.err = nil
	if _, err := orch.Book(ctx, "MF-0007"); err != nil || f.calls != 2 {
		t.Fatalf("after the failure expired: %v, %d calls", err, f.calls)
	}
	// No source at all: unchecked too.
	orch2 := freightOrder(t, &carriers{}, f, 5000)
	orch2.Floors = nil
	if _, err := orch2.Book(ctx, "MF-0007"); !errors.Is(err, ErrFloorUnchecked) {
		t.Errorf("no floor source: %v", err)
	}
}

// TestFreightCache: a floor is kept for an hour, so the plan's views
// don't each call QualP; a different leg is asked for.
func TestFreightCache(t *testing.T) {
	ctx := context.Background()
	f := &floors{}
	orch := freightOrder(t, &carriers{}, f, 1050)
	for range 3 {
		_, _ = orch.PlanChecked(ctx, "MF-0007")
	}
	if f.calls != 1 {
		t.Errorf("%d calls for one leg", f.calls)
	}
	now := orch.Now()
	orch.Now = func() time.Time { return now.Add(floorTTL + time.Second) }
	_, _ = orch.PlanChecked(ctx, "MF-0007")
	if f.calls != 2 {
		t.Errorf("%d calls after the hour", f.calls)
	}
	orch.Lanes[0].Legs[1].Freight.Axles = 6
	_, _ = orch.PlanChecked(ctx, "MF-0007")
	if f.calls != 3 {
		t.Errorf("%d calls: another axle count is another floor", f.calls)
	}
	// A caller gone isn't remembered as a failure.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	f.err = context.Canceled
	orch.Lanes[0].Legs[1].Freight.Axles = 7
	_, _ = orch.PlanChecked(cctx, "MF-0007")
	f.err = nil
	if steps, _ := orch.PlanChecked(ctx, "MF-0007"); steps[1].Freight.Status != FreightBelow {
		t.Errorf("a cancelled call was kept: %+v", steps[1].Freight)
	}
}

// TestSetRateRefusals: a rate only for a pending order's unbooked leg
// with a floor, in range.
func TestSetRateRefusals(t *testing.T) {
	ctx := context.Background()
	orch := freightOrder(t, &carriers{}, &floors{}, 1050)
	for name, tc := range map[string]struct {
		ref  string
		leg  int
		rate float64
		want error
	}{
		"unknown order": {"MF-9999", 2, 1200, ErrUnknownOrder},
		"no floor":      {"MF-0007", 1, 1200, ErrNoFreight},
		"no such leg":   {"MF-0007", 3, 1200, ErrNoFreight},
		"zero":          {"MF-0007", 2, 0, ErrNoFreight},
		"too much":      {"MF-0007", 2, config.MaxRate + 1, ErrNoFreight},
	} {
		if _, err := orch.SetRate(ctx, tc.ref, tc.leg, tc.rate); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
	// Below the floor is the person's to set; booking then refuses it.
	if steps, err := orch.SetRate(ctx, "MF-0007", 2, 900.004); err != nil || steps[1].Freight.Rate != 900 || steps[1].Freight.Status != FreightBelow {
		t.Errorf("a low rate: %+v, %v", steps, err)
	}
	if _, err := orch.SetRate(ctx, "MF-0007", 2, 1200); err != nil {
		t.Fatal(err)
	}
	if _, err := orch.Book(ctx, "MF-0007"); err != nil {
		t.Fatal(err)
	}
	if _, err := orch.SetRate(ctx, "MF-0007", 2, 1300); !errors.Is(err, ErrAnswered) {
		t.Errorf("after the answer: %v", err)
	}
	// A booked leg of a pending order (a resumed booking) keeps its rate.
	orch2 := freightOrder(t, &carriers{refuseRoad: true}, &floors{}, 1200)
	orch2.Lanes[0].Legs[0].Freight = nil
	_, _ = orch2.Book(ctx, "MF-0007") // the ship booked, the truck refused
	b, _ := orch2.Orders.Bookings.Get(meridian, "MF-0007")
	if len(b.Order.Legs) != 1 {
		t.Fatalf("legs %+v", b.Order.Legs)
	}
	orch2.Lanes[0].Legs[0].Freight = &config.LaneFreight{Rate: 1, Axles: 5, LoadType: "geral", Table: "A"}
	if _, err := orch2.SetRate(ctx, "MF-0007", 1, 1300); !errors.Is(err, ErrNoFreight) {
		t.Errorf("a booked leg's rate: %v", err)
	}
	// No lane.
	orch2.Lanes = nil
	if _, err := orch2.SetRate(ctx, "MF-0007", 2, 1300); !errors.Is(err, ErrNoLane) {
		t.Errorf("no lane: %v", err)
	}
	if _, err := orch2.PlanChecked(ctx, "MF-0007"); !errors.Is(err, ErrNoLane) {
		t.Errorf("no lane: %v", err)
	}
	if _, err := orch2.PlanChecked(ctx, "MF-9999"); !errors.Is(err, ErrUnknownOrder) {
		t.Errorf("unknown: %v", err)
	}
}

// TestControlRate: the rate through the control API, with the outcome's
// status code; the order's view shows the freight checked.
func TestControlRate(t *testing.T) {
	orch := freightOrder(t, &carriers{}, &floors{}, 1050)
	mux := http.NewServeMux()
	RegisterControl(mux, orch.Orders, orch, nil, store.NewEventLog(100))
	do := func(method, path, body string) (int, string) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		mux.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	if code, body := do(http.MethodGet, "/orders/MF-0007", ""); code != 200 || !strings.Contains(body, `"status":"below"`) {
		t.Errorf("the order: HTTP %d %s", code, body)
	}
	if code, body := do(http.MethodPost, "/orders/MF-0007/book", ""); code != http.StatusUnprocessableEntity || !strings.Contains(body, "below the ANTT minimum") {
		t.Errorf("book below: HTTP %d %s", code, body)
	}
	for _, tc := range []struct {
		path, body string
		want       int
	}{
		{"/orders/MF-0007/rate", `{"leg":2,"rate":1180}`, 200},
		{"/orders/MF-9999/rate", `{"leg":2,"rate":1180}`, 404},
		{"/orders/MF-0007/rate", `{"leg":1,"rate":1180}`, 422},
		{"/orders/MF-0007/rate", `{"leg":2,"rate":"x"}`, 400},
	} {
		if code, body := do(http.MethodPost, tc.path, tc.body); code != tc.want {
			t.Errorf("%s %s: HTTP %d %s, want %d", tc.path, tc.body, code, body, tc.want)
		}
	}
	if code, body := do(http.MethodPost, "/orders/MF-0007/book", ""); code != 200 {
		t.Errorf("book: HTTP %d %s", code, body)
	}
	if code, _ := do(http.MethodPost, "/orders/MF-0007/rate", `{"leg":2,"rate":1200}`); code != http.StatusConflict {
		t.Errorf("rate after the answer: HTTP %d", code)
	}
}

// TestTown: a door's town and state for the route, else its UN/LOCODE.
func TestTown(t *testing.T) {
	for want, p := range map[string]intents.Place{
		"Cajamar, SP": {UNLocationCode: "BRSAO", Address: &intents.Address{Locality: "Cajamar", Region: "SP", Country: "BR"}},
		"Tilburg, NL": {UNLocationCode: "NLTLB", Address: &intents.Address{Locality: "Tilburg", Country: "NL"}},
		"BRSSZ":       {UNLocationCode: "BRSSZ"},
		"BRSAO":       {UNLocationCode: "BRSAO", Address: &intents.Address{Locality: " "}},
	} {
		if got := town(p); got != want {
			t.Errorf("town(%+v) = %q, want %q", p, got, want)
		}
	}
}
