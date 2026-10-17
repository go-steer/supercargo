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

package config

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/geo"
)

// TestPlanning: a road carrier's journey planning (the route through its
// corridor, the weather), the city's curfew with its standing policy, and
// the driver app.
func TestPlanning(t *testing.T) {
	ocean := "  containerPrefix: OCBU\n  vessel: Ocean Blue Aurora\n  voyage: 42W\n  departureHour: 18\n  transit: 134h\n"
	road := `  road:
    transit: 44h
    cte: { cnpj: "12345678000190", state: 51 }
    planning:
      route: { qualp: { url: "http://127.0.0.1:8470", tokenEnv: QUALP_TOKEN, label: "QualP (simulated)" }, axles: 7, loadType: frigorificada, table: A }
      via: [{ name: Sinop, plusCode: "58W64FPV+89" }]
      weather: { google: { apiKeyEnv: MAPS_API_KEY, language: pt-BR } }
    curfew: { zone: "São Paulo truck restriction", locodes: [BRSAO, BRGRU], from: "05:00", to: "21:00", timeZone: America/Sao_Paulo, yard: "Guarulhos yard", yardToDelivery: 90m, by: "Dispatch lead" }
    driverApp: { mock: "Trizy (simulated)" }
`
	in := strings.Replace(operatorYAML, ocean, road, 1)
	c, err := Parse([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	r := c.Operator.Road
	p, cf := r.Planning, r.Curfew
	if p.Route.Axles != 7 || p.Via[0].Name != "Sinop" || p.Weather.Google.Language != "pt-BR" || r.DriverApp.Mock != "Trizy (simulated)" || cf.YardToDelivery != 90*time.Minute {
		t.Errorf("%+v %+v %+v", p, cf, r.DriverApp)
	}
	if from, to, loc := cf.Window(); from != 300 || to != 1260 || loc.String() != "America/Sao_Paulo" {
		t.Errorf("window %d-%d %v", from, to, loc)
	}
	mock := strings.Replace(in, "weather: { google: { apiKeyEnv: MAPS_API_KEY, language: pt-BR } }", `weather: { mock: "Weather (simulated)" }`, 1)
	if c, err := Parse([]byte(mock)); err != nil || c.Operator.Road.Planning.Weather.Mock != "Weather (simulated)" {
		t.Errorf("mock weather: %v", err)
	}
	for name, edit := range map[string][2]string{
		"weather and nothing":   {"      weather: { google: { apiKeyEnv: MAPS_API_KEY, language: pt-BR } }\n", "      weather: {}\n"},
		"a route without qualp": {`route: { qualp: { url: "http://127.0.0.1:8470", tokenEnv: QUALP_TOKEN, label: "QualP (simulated)" },`, "route: {"},
		"qualp over http":       {`url: "http://127.0.0.1:8470"`, `url: "http://qualp.example"`},
		"eight axles":           {"axles: 7", "axles: 8"},
		"a short code":          {`plusCode: "58W64FPV+89"`, `plusCode: "4FPV+8C"`},
		"a nameless point":      {"name: Sinop", `name: ""`},
		"both weathers":         {"language: pt-BR } }", `language: pt-BR }, mock: "x" }`},
		"a bidi mock":           {"weather: { google: { apiKeyEnv: MAPS_API_KEY, language: pt-BR } }", "weather: { mock: \"a\u202eb\" }"},
		"a bad key variable":    {"apiKeyEnv: MAPS_API_KEY", "apiKeyEnv: 9X"},
		"a bad language":        {"language: pt-BR", "language: portuguese"},
		"a bidi driver app":     {`mock: "Trizy (simulated)"`, "mock: \"a\u202eb\""},
		"no zone":               {`zone: "São Paulo truck restriction"`, `zone: ""`},
		"a backwards window":    {`from: "05:00", to: "21:00"`, `from: "21:00", to: "05:00"`},
		"an hour without zero":  {`from: "05:00"`, `from: "5:00"`},
		"no yard time":          {"yardToDelivery: 90m", "yardToDelivery: 0s"},
		"a day in the yard":     {"yardToDelivery: 90m", "yardToDelivery: 24h"},
		"into the next window":  {"yardToDelivery: 90m", "yardToDelivery: 8h"},
		"a padded code":         {`plusCode: "58W64FPV+89"`, `plusCode: "58000000+"`},
		"a driver app alone":    {`    curfew: { zone: "São Paulo truck restriction", locodes: [BRSAO, BRGRU], from: "05:00", to: "21:00", timeZone: America/Sao_Paulo, yard: "Guarulhos yard", yardToDelivery: 90m, by: "Dispatch lead" }` + "\n", ""},
		"no locodes":            {"locodes: [BRSAO, BRGRU]", "locodes: []"},
		"a bad locode":          {"locodes: [BRSAO, BRGRU]", "locodes: [BRSAO, guarulhos]"},
		"an unknown zone":       {"timeZone: America/Sao_Paulo", "timeZone: Mars/Olympus"},
		"the local zone":        {"timeZone: America/Sao_Paulo", "timeZone: Local"},
	} {
		bad := strings.Replace(in, edit[0], edit[1], 1)
		if bad == in {
			t.Fatalf("%s: edit didn't apply", name)
		}
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// The example's Sinop is Sinop (within 2 km of the town's centre).
	if pt, err := geo.Decode(p.Via[0].PlusCode); err != nil || math.Abs(pt.Lat+11.8642) > 0.02 || math.Abs(pt.Lng+55.5066) > 0.02 {
		t.Errorf("Sinop: %v %v", pt, err)
	}
	hant := strings.Replace(in, "language: pt-BR", "language: zh-Hant", 1)
	if _, err := Parse([]byte(hant)); err != nil {
		t.Errorf("zh-Hant: %v", err)
	}
	nothing := strings.Replace(strings.Replace(in, "      route: { qualp: { url: \"http://127.0.0.1:8470\", tokenEnv: QUALP_TOKEN, label: \"QualP (simulated)\" }, axles: 7, loadType: frigorificada, table: A }\n", "", 1),
		"      weather: { google: { apiKeyEnv: MAPS_API_KEY, language: pt-BR } }\n", "", 1)
	if _, err := Parse([]byte(nothing)); err == nil || !strings.Contains(err.Error(), "needs a route") {
		t.Errorf("nothing to plan: %v", err)
	}
	many := strings.Replace(in, `via: [{ name: Sinop, plusCode: "58W64FPV+89" }]`, "via: ["+strings.Repeat(`{ name: Sinop, plusCode: "58W64FPV+89" }, `, MaxWaypoints+1)+"]", 1)
	if _, err := Parse([]byte(many)); err == nil {
		t.Error("too many points: accepted")
	}
}
