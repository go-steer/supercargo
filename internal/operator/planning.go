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
	"math"
	"strings"
	"sync"
	"time"

	"github.com/go-steer/supercargo/internal/backend"
	"github.com/go-steer/supercargo/internal/geo"
	"github.com/go-steer/supercargo/internal/safetext"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/intents"
)

// Planning is how a road carrier plans a leg's journey when it books one
// (docs/DESIGN.md, "Journey planning"): the heavy-truck route through its
// corridor (QualP: distance, driving time and tolls for the vehicle), and
// the weather along it at the hours the truck should pass. Both are
// advice for the carrier's people: a step says what each found, or that
// it couldn't, and the booking goes ahead either way. Nothing of it goes
// in an envelope.
type Planning struct {
	Route backend.FreightFloors // optional
	// Vehicle prices the route: its loaded axles and the ANTT's load
	// type and table.
	Axles    int
	LoadType string
	Table    string
	// Via is the carrier's corridor, between pickup and delivery.
	Via     []Waypoint
	Weather backend.Weather // optional
	// RainWarn is the chance of rain (percent) at which a point is
	// called out (default 60).
	RainWarn int
	// Budget is the time planning may take (default Budget).
	Budget time.Duration
	// OnError learns why a service couldn't answer (the step says only
	// "not available"). Optional; called from several goroutines.
	OnError func(system string, err error)
}

// Waypoint is a named point on the corridor, e.g. Sinop by its Plus Code.
type Waypoint struct {
	Name  string
	Point geo.Point
}

// pointOf is a leg's end's point, if its Plus Code gives one.
func pointOf(s intents.Stop) *geo.Point {
	if p, err := geo.Decode(s.PlusCode); err == nil {
		return &p
	}
	return nil
}

// location is how the route service is told a stop: its point, else its
// town, else its UN/LOCODE.
func location(s intents.Stop) string {
	if p := pointOf(s); p != nil {
		return p.String()
	}
	if a := s.Address; a != nil && a.Locality != "" {
		return strings.TrimSuffix(a.Locality+", "+a.Region, ", ")
	}
	return s.UNLocationCode
}

// Budget is the default time planning may take, all of it: it runs
// while the booking waits, and the shipper's request has its own
// deadline.
const Budget = 4 * time.Second

// maxLookups is how many weather lookups run at once.
const maxLookups = 4

// steps plans the journey of b, from pickup to delivery, within the
// budget: what isn't answered by then is "not available".
func (p *Planning) steps(ctx context.Context, b *intents.RoadBookingRequestBody, pickup, delivery, now time.Time) []store.BackOfficeStep {
	if p == nil {
		return nil
	}
	budget := p.Budget
	if budget <= 0 {
		budget = Budget
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	var route store.BackOfficeStep
	done := make(chan struct{})
	go func() {
		defer close(done)
		if p.Route != nil {
			route = p.route(ctx, b, now)
		}
	}()
	sky, wx := store.BackOfficeStep{}, false
	if p.Weather != nil {
		sky, wx = p.weather(ctx, b, pickup, delivery, now)
	}
	<-done
	var out []store.BackOfficeStep
	if p.Route != nil {
		out = append(out, route)
	}
	if wx {
		out = append(out, sky)
	}
	return out
}

func (p *Planning) fail(system string, err error) {
	if p.OnError != nil {
		p.OnError(system, err)
	}
}

func (p *Planning) route(ctx context.Context, b *intents.RoadBookingRequestBody, now time.Time) store.BackOfficeStep {
	locs := []string{location(b.Pickup)}
	var via []string
	for _, w := range p.Via {
		locs, via = append(locs, w.Point.String()), append(via, w.Name)
	}
	locs = append(locs, location(b.Delivery))
	step := store.BackOfficeStep{System: "route", Source: "QualP", Action: "Heavy-truck route not available", At: now}
	f, err := p.Route.Floor(ctx, backend.FloorRequest{Locations: locs, Axles: p.Axles, LoadType: p.LoadType, Table: p.Table})
	if err != nil {
		p.fail("route", err)
		return step
	}
	step.Source, step.Action = f.Source, "Heavy-truck route planned"
	step.Reference = fmt.Sprintf("%.0f km · %s driving · %d toll plazas, R$ %.2f (%d axles)",
		f.DistanceKm, hm(f.Duration), len(f.Tolls), f.TollTotal, p.Axles)
	if len(via) > 0 {
		step.Detail = "via " + strings.Join(via, ", ")
	}
	return step
}

// weather checks each point of the corridor that has coordinates. With
// both ends' points, each at the hour the truck should pass it (spread
// over the leg by distance); without them those hours aren't known, so
// the current conditions.
func (p *Planning) weather(ctx context.Context, b *intents.RoadBookingRequestBody, pickup, delivery, now time.Time) (store.BackOfficeStep, bool) {
	from, to := pointOf(b.Pickup), pointOf(b.Delivery)
	points := []Waypoint{}
	if from != nil {
		points = append(points, Waypoint{"pickup", *from})
	}
	points = append(points, p.Via...)
	if to != nil {
		points = append(points, Waypoint{"delivery", *to})
	}
	if len(points) == 0 {
		return store.BackOfficeStep{}, false
	}
	timed := from != nil && to != nil
	// Distances along the corridor, from the pickup.
	along, total := make([]float64, len(points)), 0.0
	for i := 1; i < len(points); i++ {
		total += km(points[i-1].Point, points[i].Point)
		along[i] = total
	}
	answers := make([]*backend.Conditions, len(points))
	sem := make(chan struct{}, maxLookups)
	var wg sync.WaitGroup
	for i, w := range points {
		at := time.Time{} // the current conditions
		if timed {
			at = pickup
			if total > 0 {
				at = pickup.Add(time.Duration(float64(delivery.Sub(pickup)) * along[i] / total))
			}
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			c, err := p.Weather.At(ctx, w.Point, at)
			if err == nil && (c == nil || safetext.Problem(c.Source, 64) != "" || safetext.Problem(c.Description, 64) != "") {
				err = errors.New("the answer is missing or not safe to show")
			}
			if err != nil {
				p.fail("weather", err)
				return
			}
			answers[i] = c
		}()
	}
	wg.Wait()
	warn := p.RainWarn
	if warn == 0 {
		warn = 60
	}
	step := store.BackOfficeStep{System: "weather", Action: "Weather checked along the corridor", At: now}
	var parts, wet []string
	for i, c := range answers {
		if c == nil {
			continue
		}
		step.Source = c.Source
		when := "now"
		if c.Forecast {
			when = c.At.In(brasilia).Format("Jan 2 15:04 BRT")
		}
		parts = append(parts, fmt.Sprintf("%s %s: %s, %.0f°C, rain %d%%", points[i].Name, when, c.Description, c.TempC, c.RainChance))
		if c.RainChance >= warn {
			wet = append(wet, points[i].Name)
		}
	}
	if len(parts) == 0 {
		step.Source, step.Action = "weather", "Weather not available"
		return step, true
	}
	if len(wet) > 0 {
		step.Action = "Weather checked: rain likely near " + strings.Join(wet, ", ")
	}
	step.Reference = clip(strings.Join(parts, " · "), 400)
	return step, true
}

// hm is a duration as "31h20".
func hm(d time.Duration) string {
	m := int(d.Round(time.Minute) / time.Minute)
	return fmt.Sprintf("%dh%02d", m/60, m%60)
}

// km is the straight-line distance between two points.
func km(a, b geo.Point) float64 {
	rad := func(d float64) float64 { return d * math.Pi / 180 }
	dlat, dlng := rad(b.Lat-a.Lat), rad(b.Lng-a.Lng)
	h := math.Sin(dlat/2)*math.Sin(dlat/2) + math.Cos(rad(a.Lat))*math.Cos(rad(b.Lat))*math.Sin(dlng/2)*math.Sin(dlng/2)
	return 2 * 6371 * math.Asin(math.Sqrt(h))
}
