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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/backend"
	"github.com/go-steer/supercargo/internal/geo"
	"github.com/go-steer/supercargo/internal/weather"
	"github.com/go-steer/supercargo/pkg/intents"
)

// routes is a route service that answers a fixed route, or fails.
type routes struct {
	asked []backend.FloorRequest
	err   error
}

func (r *routes) Floor(_ context.Context, req backend.FloorRequest) (*backend.Floor, error) {
	r.asked = append(r.asked, req)
	if r.err != nil {
		return nil, r.err
	}
	return &backend.Floor{DistanceKm: 2361.3, Duration: 42*time.Hour + 56*time.Minute, TollTotal: 461.3,
		Tolls: make([]backend.Toll, 6), Source: "QualP (simulated)"}, nil
}

// skies is weather that fails for some points, or answers text that
// isn't safe to show.
type skies struct {
	fail   map[float64]bool
	unsafe bool
}

func (s *skies) At(_ context.Context, p geo.Point, at time.Time) (*backend.Conditions, error) {
	if s.fail[p.Lat] {
		return nil, errors.New("no weather")
	}
	d := "Clear"
	if s.unsafe {
		d = "Clear\u202e"
	}
	return &backend.Conditions{Description: d, TempC: 25, At: at, Source: "Skies"}, nil
}

var (
	sinop       = geo.Point{Lat: -11.8642, Lng: -55.5066}
	cuiaba      = geo.Point{Lat: -15.6014, Lng: -56.0979}
	guarulhosDC = geo.Point{Lat: -23.4628, Lng: -46.5333}
)

// truckload is the FTL pilot's leg: from a farm by its Plus Code to a DC
// in Guarulhos by its own.
func truckload() *intents.RoadBookingRequestBody {
	b := roadBody()
	b.Pickup = intents.Stop{Facility: "Fazenda Boa Vista", UNLocationCode: "BRCGB", PlusCode: "58V75885+8J", Earliest: "2026-10-24T14:00:00Z"}
	b.Delivery = intents.Stop{Facility: "Aurora DC", UNLocationCode: "BRGRU", PlusCode: geo.Encode(guarulhosDC), Latest: "2026-10-27T12:00:00Z"}
	return b
}

// TestPlanning: a booked leg's route goes through the corridor by its
// points, and the weather is checked at each when the truck passes: the
// farm at the pickup, the DC at the delivery, the rest in between.
func TestPlanning(t *testing.T) {
	r := &routes{}
	tb := trucks()
	tb.Planning = &Planning{Route: r, Axles: 7, LoadType: "frigorificada", Table: "A",
		Via: []Waypoint{{"Sinop", sinop}, {"Cuiabá", cuiaba}}, Weather: &weather.Mock{Source: "Weather (simulated)"}}
	conf, err := tb.Decide(context.Background(), backend.RoadBookingRequest{Body: truckload()})
	if err != nil || conf.Status != intents.StatusConfirmed {
		t.Fatalf("%+v %v", conf, err)
	}
	if len(r.asked) != 1 {
		t.Fatalf("asked %+v", r.asked)
	}
	locs := r.asked[0].Locations
	if len(locs) != 4 || locs[0] != "-12.83419,-54.69094" || locs[1] != "-11.86420,-55.50660" || !strings.HasPrefix(locs[3], "-23.46") || r.asked[0].Axles != 7 || r.asked[0].LoadType != "frigorificada" {
		t.Errorf("asked %+v", r.asked)
	}
	steps := tb.BackOffice(conf.BookingRef)
	if len(steps) != 3 || steps[2].System != "tms" {
		t.Fatalf("steps %+v", steps)
	}
	route, sky := steps[0], steps[1]
	if route.System != "route" || route.Source != "QualP (simulated)" || route.Action != "Heavy-truck route planned" ||
		route.Reference != "2361 km · 42h56 driving · 6 toll plazas, R$ 461.30 (7 axles)" || route.Detail != "via Sinop, Cuiabá" {
		t.Errorf("route %+v", route)
	}
	// Picked up 14:00 UTC (11:00 in Brasília), delivered 30h later.
	if sky.System != "weather" || sky.Source != "Weather (simulated)" || sky.Action != "Weather checked: rain likely near pickup, Sinop" ||
		!strings.HasPrefix(sky.Reference, "pickup Oct 24 11:00 BRT: Thunderstorms, 31°C, rain 70% · Sinop Oct 24 13:00 BRT") ||
		!strings.Contains(sky.Reference, "delivery Oct 25 17:00 BRT: Partly cloudy, 27°C, rain 10%") {
		t.Errorf("weather %+v", sky)
	}
}

// TestPlanningAdvice: planning is advice: a route service that fails, or
// weather that fails or answers text unsafe to show, is said, and the leg
// is booked all the same.
func TestPlanningAdvice(t *testing.T) {
	for name, c := range map[string]struct {
		p     *Planning
		steps string
	}{
		"no route":           {&Planning{Route: &routes{err: errors.New("down")}}, "route:Heavy-truck route not available"},
		"some weather":       {&Planning{Weather: &skies{fail: map[float64]bool{sinop.Lat: true}}, Via: []Waypoint{{"Sinop", sinop}, {"Cuiabá", cuiaba}}}, "weather:Weather checked along the corridor"},
		"no weather":         {&Planning{Weather: &skies{fail: map[float64]bool{sinop.Lat: true}}, Via: []Waypoint{{"Sinop", sinop}}}, "weather:Weather not available"},
		"unsafe weather":     {&Planning{Weather: &skies{unsafe: true}, Via: []Waypoint{{"Sinop", sinop}}}, "weather:Weather not available"},
		"no points to check": {&Planning{Weather: &skies{}}, ""},
		"no ends' points":    {&Planning{Weather: &weather.Mock{Source: "W"}, Via: []Waypoint{{"Sinop", sinop}}}, "weather:Weather checked: rain likely near Sinop"},
	} {
		tb := trucks()
		tb.CTe = nil
		tb.Planning = c.p
		conf, err := tb.Decide(context.Background(), backend.RoadBookingRequest{Body: roadBody()})
		if err != nil || conf.Status != intents.StatusConfirmed {
			t.Fatalf("%s: %+v %v", name, conf, err)
		}
		var got []string
		for _, s := range tb.BackOffice(conf.BookingRef) {
			got = append(got, s.System+":"+s.Action)
			if name == "some weather" && (strings.Contains(s.Reference, "Sinop") || !strings.HasPrefix(s.Reference, "Cuiabá now: Clear")) {
				t.Errorf("%s: %q", name, s.Reference)
			}
			// Without the ends' points the hours aren't known: now.
			if name == "no ends' points" && !strings.HasPrefix(s.Reference, "Sinop now: Thunderstorms") {
				t.Errorf("%s: %q", name, s.Reference)
			}
		}
		if strings.Join(got, ",") != c.steps {
			t.Errorf("%s: steps %q", name, got)
		}
	}
	var none *Planning
	if s := none.steps(context.Background(), roadBody(), roadNow, roadNow, roadNow); s != nil {
		t.Errorf("no planning: %+v", s)
	}
}

// hanging is a service that answers only when its caller gives up.
type hanging struct{}

func (hanging) Floor(ctx context.Context, _ backend.FloorRequest) (*backend.Floor, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (hanging) At(ctx context.Context, _ geo.Point, _ time.Time) (*backend.Conditions, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestPlanningBudget: services that hang hold the booking no longer than
// the budget; each step says "not available", and why reaches OnError.
// What planning finds stays out of the confirmation.
func TestPlanningBudget(t *testing.T) {
	var mu sync.Mutex
	var why []string
	tb := trucks()
	tb.Planning = &Planning{Route: hanging{}, Weather: hanging{}, Via: []Waypoint{{"Sinop", sinop}, {"Cuiabá", cuiaba}}, Budget: 100 * time.Millisecond,
		OnError: func(system string, err error) {
			mu.Lock()
			defer mu.Unlock()
			why = append(why, system+": "+err.Error())
		}}
	start := time.Now()
	conf, err := tb.Decide(context.Background(), backend.RoadBookingRequest{Body: truckload()})
	if took := time.Since(start); err != nil || took > 2*time.Second {
		t.Fatalf("%v after %v", err, took)
	}
	var got []string
	for _, s := range tb.BackOffice(conf.BookingRef) {
		got = append(got, s.Action)
	}
	if strings.Join(got, ",") != "Heavy-truck route not available,Weather not available,CT-e issued (authorized by SEFAZ)" || len(why) != 5 {
		t.Errorf("steps %q, why %q", got, why)
	}
	tb.Planning = &Planning{Route: &routes{}, Axles: 7, Weather: &weather.Mock{Source: "Weather (simulated)"}}
	conf, _ = tb.Decide(context.Background(), backend.RoadBookingRequest{Body: truckload()})
	raw, _ := json.Marshal(conf)
	for _, private := range []string{"QualP", "toll", "Thunderstorms", "Weather", "km"} {
		if strings.Contains(string(raw), private) {
			t.Errorf("the confirmation carries %q: %s", private, raw)
		}
	}
}

// TestLocation: a stop goes to the route service by its point, else its
// town, else its UN/LOCODE; a code that isn't a full one gives no point.
func TestLocation(t *testing.T) {
	for want, s := range map[string]intents.Stop{
		"-12.83419,-54.69094": {PlusCode: "58V75885+8J", UNLocationCode: "BRCGB"},
		"Guarulhos, SP":       {Address: &intents.Address{Locality: "Guarulhos", Region: "SP"}, UNLocationCode: "BRGRU"},
		"Guarulhos":           {Address: &intents.Address{Locality: "Guarulhos"}, UNLocationCode: "BRGRU"},
		"BRGRU":               {PlusCode: "5885+8J", UNLocationCode: "BRGRU"},
	} {
		if got := location(s); got != want {
			t.Errorf("%+v: %q, want %q", s, got, want)
		}
	}
	if hm(90*time.Second) != "0h02" || hm(31*time.Hour+20*time.Minute) != "31h20" {
		t.Error("hm")
	}
}
