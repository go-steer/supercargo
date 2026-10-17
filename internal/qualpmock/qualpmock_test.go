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

package qualpmock_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-steer/supercargo/internal/antt"
	"github.com/go-steer/supercargo/internal/backend"
	"github.com/go-steer/supercargo/internal/qualp"
	"github.com/go-steer/supercargo/internal/qualpmock"
)

func client(t *testing.T, token string) *qualp.Client {
	t.Helper()
	srv := httptest.NewServer((&qualpmock.Server{Token: "t0ken"}).Handler())
	t.Cleanup(srv.Close)
	return &qualp.Client{BaseURL: srv.URL, Token: token, Source: "simulated qualp"}
}

// TestSantosToCajamar: the demo's Brazilian leg, through the real client
// against the mock: a route with its toll plazas in order, their fares for
// five axles, and the ANTT floor for its distance.
func TestSantosToCajamar(t *testing.T) {
	f, err := client(t, "t0ken").Floor(context.Background(), backend.FloorRequest{
		Locations: []string{"Santos, SP", "Cajamar / SP"}, Axles: 5, LoadType: "conteineirizada", Table: "A"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%.1f km, %s, tolls R$ %.2f, floor R$ %.2f (loading R$ %.2f)", f.DistanceKm, f.Duration, f.TollTotal, f.Freight, f.LoadUnload)
	if f.DistanceKm < 100 || f.DistanceKm > 140 || f.Duration <= 0 || f.Source != "simulated qualp" {
		t.Errorf("route %+v", f)
	}
	var names []string
	total := 0.0
	for _, p := range f.Tolls {
		names = append(names, p.Name)
		total += p.Amount
	}
	if strings.Join(names, ",") != "Riacho Grande,Rodoanel Oeste,Perus" || f.TollTotal == 0 || f.Tolls[0].Amount != 179.5 {
		t.Errorf("tolls %v, total %.2f: %+v", names, f.TollTotal, f.Tolls)
	}
	want, cc, _ := antt.Floor("A", "conteineirizada", 5, f.DistanceKm)
	if f.Freight != want || f.LoadUnload != cc || !strings.Contains(f.Resolution, "6.034") || f.ResolutionURL != antt.Resolution.URL {
		t.Errorf("floor %+v, want %.2f", f, want)
	}
}

// TestFarmToGuarulhos: the FTL pilot's truckload, from a farm known only
// by its point, up BR-163 and on to a DC in Guarulhos: Mato Grosso's toll
// plazas in order, and the point named by its coordinates.
func TestFarmToGuarulhos(t *testing.T) {
	f, err := client(t, "t0ken").Floor(context.Background(), backend.FloorRequest{
		Locations: []string{"-12.83419,-54.69094", "Sinop, MT", "Cuiabá/MT", "Rondonópolis", "Guarulhos, SP"}, Axles: 7, LoadType: "frigorificada", Table: "A"})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range f.Tolls {
		names = append(names, p.Name)
	}
	if f.DistanceKm < 2100 || f.DistanceKm > 2400 || strings.Join(names, ",") != "Sorriso,Nova Mutum,Jangada,Campo Verde,Valinhos,Campo Limpo" || f.Tolls[0].Amount != 65.8 {
		t.Errorf("%.1f km, tolls %v: %+v", f.DistanceKm, names, f.Tolls)
	}
	for _, bad := range []string{"-12.8,-54.6,", "12.8;-54.6", "48.85,2.35", "-12.8,-154.6"} {
		if _, err := client(t, "t0ken").Floor(context.Background(), backend.FloorRequest{
			Locations: []string{bad, "Guarulhos"}, Axles: 7, LoadType: "frigorificada", Table: "A"}); err == nil {
			t.Errorf("%q: a route", bad)
		}
	}
}

// TestRefusals: a token the mock doesn't take, a place it doesn't know,
// and a combination the ANTT doesn't price fail.
func TestRefusals(t *testing.T) {
	ctx := context.Background()
	leg := backend.FloorRequest{Locations: []string{"Santos", "Cajamar"}, Axles: 5, LoadType: "conteineirizada", Table: "A"}
	if _, err := client(t, "wrong").Floor(ctx, leg); !errors.Is(err, qualp.ErrUnauthorized) {
		t.Errorf("a wrong token: %v", err)
	}
	if _, err := client(t, "").Floor(ctx, leg); !errors.Is(err, qualp.ErrUnauthorized) {
		t.Errorf("no token: %v", err)
	}
	c := client(t, "t0ken")
	for name, r := range map[string]backend.FloorRequest{
		"an unknown place":      {Locations: []string{"Santos", "Atlantis"}, Axles: 5, LoadType: "geral", Table: "A"},
		"containers on 2 axles": {Locations: []string{"Santos", "Cajamar"}, Axles: 2, LoadType: "conteineirizada", Table: "A"},
		"table E":               {Locations: []string{"Santos", "Cajamar"}, Axles: 5, LoadType: "geral", Table: "E"},
		"one place":             {Locations: []string{"Santos"}, Axles: 5, LoadType: "geral", Table: "A"},
		"ten axles":             {Locations: []string{"Santos", "Cajamar"}, Axles: 10, LoadType: "geral", Table: "A"},
	} {
		if _, err := c.Floor(ctx, r); err == nil {
			t.Errorf("%s: priced", name)
		}
	}
}

// TestFreightByLocations: /tabela-frete/v1 takes locations or a distance,
// exclusively, and needs is_empty_return; the empty-return rule isn't
// simulated.
func TestFreightByLocations(t *testing.T) {
	srv := httptest.NewServer((&qualpmock.Server{}).Handler())
	t.Cleanup(srv.Close)
	for name, tc := range map[string]struct {
		body string
		want int
	}{
		"by locations":     {`{"locations":["Santos","Cajamar"],"axis":5,"freight_type":"A","load_type":"geral","is_empty_return":false}`, 200},
		"by distance":      {`{"distance":100,"axis":2,"freight_type":"A","load_type":"granel_solido","is_empty_return":false}`, 200},
		"both":             {`{"distance":100,"locations":["Santos","Cajamar"],"axis":2,"freight_type":"A","load_type":"geral","is_empty_return":false}`, 422},
		"neither":          {`{"axis":2,"freight_type":"A","load_type":"geral","is_empty_return":false}`, 422},
		"no empty return":  {`{"distance":100,"axis":2,"freight_type":"A","load_type":"geral"}`, 422},
		"empty return":     {`{"distance":100,"axis":2,"freight_type":"A","load_type":"geral","is_empty_return":true}`, 422},
		"unknown location": {`{"locations":["Santos","Atlantis"],"axis":5,"freight_type":"A","load_type":"geral","is_empty_return":false}`, 422},
		"not JSON":         {`{`, 422},
	} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/tabela-frete/v1", strings.NewReader(tc.body))
		req.Header.Set("Access-Token", "any")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%s: HTTP %d, want %d", name, resp.StatusCode, tc.want)
		}
	}
	for _, path := range []string{"/rotas/v4"} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(`{`))
		req.Header.Set("Access-Token", "any")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 422 {
			t.Errorf("%s not JSON: HTTP %d", path, resp.StatusCode)
		}
	}
}
