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

package app

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/config"
	"github.com/go-steer/supercargo/internal/qualpmock"
	"github.com/go-steer/supercargo/internal/weather"
	"github.com/go-steer/supercargo/pkg/intents"
)

// TestJourneyPlanningFromConfig: a road carrier configured to plan plans
// each leg it books (the route through its corridor, the weather), and a
// delay into the city's truck restriction is re-planned by its standing
// policy: the signed alert says so, and the leg records the policy and
// the driver's instruction.
func TestJourneyPlanningFromConfig(t *testing.T) {
	q := httptest.NewServer((&qualpmock.Server{Token: "t0ken"}).Handler())
	defer q.Close()
	t.Setenv("QUALP_TOKEN", "t0ken")
	p := startPair(t, `, road: { transit: 30h, cte: { cnpj: "12345678000190", state: 51 },
    planning: { route: { qualp: { url: "`+q.URL+`", tokenEnv: QUALP_TOKEN, label: "QualP (simulated)" }, axles: 7, loadType: frigorificada, table: A },
      via: [{ name: Sinop, plusCode: "58W64FPV+89" }], weather: { mock: "Weather (simulated)" } },
    curfew: { zone: "São Paulo truck restriction", locodes: [BRSAO], from: "05:00", to: "21:00", timeZone: America/Sao_Paulo, yard: "Guarulhos yard", yardToDelivery: 90m, by: "Dispatch lead" },
    driverApp: { mock: "Trizy (simulated)" } }`)
	ctx := context.Background()
	body := &intents.RoadBookingRequestBody{
		ShipperRef: "CY-L3",
		Shipper:    intents.Party{Name: "Cymbal Shoes", Domain: "cymbal-shoes.example"},
		Pickup:     intents.Stop{Facility: "Fazenda Boa Vista", UNLocationCode: "BRCGB", PlusCode: "58V75885+8J"},
		Delivery:   intents.Stop{Facility: "Cymbal Calçados DC", UNLocationCode: "BRSAO"},
		Equipment:  []intents.Equipment{{ISOType: "45R1", Count: 1, GrossWeightKg: 26000}},
		Cargo:      intents.Cargo{Description: "Chilled beef", HSCode: "0201"},
	}
	h := intents.HeaderFor(intents.RoadBookingRequest, "cymbal-shoes.example", "ocean-blue.example", time.Now())
	resp, err := p.shipper.Node.Exchange(ctx, h, body, intents.RoadBookingConfirmation)
	if err != nil {
		t.Fatal(err)
	}
	c := resp.Body.(*intents.RoadBookingConfirmationBody)
	b, err := p.operator.Carrier.Bookings.Get("ocean-blue.example", c.BookingRef)
	if err != nil || b.Road == nil || len(b.Road.BackOffice) != 3 {
		t.Fatalf("stored %+v, %v", b.Road, err)
	}
	route, sky := b.Road.BackOffice[0], b.Road.BackOffice[1]
	if route.Source != "QualP (simulated)" || route.Action != "Heavy-truck route planned" || route.Detail != "via Sinop" ||
		sky.Source != "Weather (simulated)" || !strings.HasPrefix(sky.Action, "Weather checked: rain likely near pickup, Sinop") {
		t.Errorf("planning %+v", b.Road.BackOffice)
	}
	// A delay that brings the truck in at 06:00 in São Paulo.
	loc, _ := time.LoadLocation("America/Sao_Paulo")
	arrival, _ := time.Parse(time.RFC3339, c.PlannedDelivery)
	l := arrival.In(loc)
	six := time.Date(l.Year(), l.Month(), l.Day()+1, 6, 0, 0, 0, loc)
	alert, err := p.operator.Simulator.Disrupt(c.BookingRef, six.Sub(arrival), "Heavy rain on BR-163 near Sinop.")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(six.Year(), six.Month(), six.Day(), 22, 30, 0, 0, loc).UTC().Format(time.RFC3339)
	if alert.NewETA != want || !strings.Contains(alert.Summary, "by standing policy it waits at Guarulhos yard until 21:00") {
		t.Errorf("alert %+v, want %s", alert, want)
	}
	b, _ = p.operator.Carrier.Bookings.Get("ocean-blue.example", c.BookingRef)
	steps := b.Road.BackOffice
	if len(steps) != 5 || steps[3].System != "policy" || steps[3].Source != "Dispatch lead" || steps[4].Source != "Trizy (simulated)" ||
		steps[4].Action != "Driver told: go to Guarulhos yard, leave at 21:00" {
		t.Errorf("steps %+v", steps)
	}
}

// TestPlanningCredentials: the route's token and the weather's key come
// from the environment; an empty one stops the node.
func TestPlanningCredentials(t *testing.T) {
	route := &config.RoutePlanning{QualP: &config.QualPSource{URL: "https://qualp.example", TokenEnv: "SC_TEST_QUALP"}, Axles: 7, LoadType: "frigorificada", Table: "A"}
	google := &config.WeatherSource{Google: &config.GoogleWeather{APIKeyEnv: "SC_TEST_MAPS", Language: "pt-BR"}}
	t.Setenv("SC_TEST_QUALP", "")
	t.Setenv("SC_TEST_MAPS", "")
	if _, err := planning(&config.Planning{Route: route}); err == nil || !strings.Contains(err.Error(), "$SC_TEST_QUALP") {
		t.Errorf("no token: %v", err)
	}
	if _, err := planning(&config.Planning{Weather: google}); err == nil || !strings.Contains(err.Error(), "$SC_TEST_MAPS") {
		t.Errorf("no key: %v", err)
	}
	t.Setenv("SC_TEST_QUALP", "t")
	t.Setenv("SC_TEST_MAPS", "k")
	pl, err := planning(&config.Planning{Route: route, Weather: google})
	if err != nil || pl.Route == nil {
		t.Fatal(err)
	}
	if g, ok := pl.Weather.(*weather.Google); !ok || g.Key != "k" || g.Language != "pt-BR" {
		t.Errorf("weather %+v", pl.Weather)
	}
}
