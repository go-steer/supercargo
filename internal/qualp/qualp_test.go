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

package qualp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-steer/supercargo/internal/backend"
)

// fake answers /rotas/v4 with route and /tabela-frete/v1 with freight
// (raw JSON), or status with body, and records what it was sent.
type fake struct {
	route, freight string
	status         int
	body           string
	got            map[string]map[string]any
	header         http.Header
	calls          atomic.Int32
}

func (f *fake) serve(t *testing.T) *Client {
	t.Helper()
	f.got = map[string]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		f.header = r.Header
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		f.got[r.URL.Path] = body
		if f.status != 0 {
			w.WriteHeader(f.status)
			_, _ = io.WriteString(w, f.body)
			return
		}
		if r.URL.Path == "/rotas/v4" {
			_, _ = io.WriteString(w, f.route)
			return
		}
		_, _ = io.WriteString(w, f.freight)
	}))
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL + "/", Token: "t"}
}

const okRoute = `{"distancia":{"texto":"118 km","valor":117.6},"duracao":{"texto":"02:08:17","valor":7697},
  "pedagios":[{"nome":"Riacho Grande","concessionaria":"Ecovias","uf":"SP","rodovia":"SP-150","tarifa":{"5":179.5,"6":215.4}},
              {"nome":"Perus","concessionaria":"CCR AutoBAn","uf":"SP","rodovia":"SP-330","tarifa":{"5":49}}],"extra":1}`
const okFreight = `{"params":{"distance":117.6,"axis":5,"freight_type":"A","load_type":"conteineirizada","is_empty_return":false},
  "data":{"freight_cost":1140.01,"load_unload_cost":497.66},
  "antt_resolution":{"name":"RESOLUÇÃO ANTT Nº 6.034","url":"https://www.in.gov.br/x"}}`

var leg = backend.FloorRequest{Locations: []string{"Santos, SP", "Cajamar, SP"}, Axles: 5, LoadType: "conteineirizada", Table: "A"}

// TestFloor: what's sent is QualP's documented body, with the token in
// Access-Token and the heavy defaults turned off; what comes back is
// checked, and the fare for the axles asked taken.
func TestFloor(t *testing.T) {
	f := &fake{route: okRoute, freight: okFreight}
	got, err := f.serve(t).Floor(context.Background(), leg)
	if err != nil {
		t.Fatal(err)
	}
	if f.header.Get("Access-Token") != "t" || got.Source != "qualp" {
		t.Errorf("token %q, source %q", f.header.Get("Access-Token"), got.Source)
	}
	route := f.got["/rotas/v4"]
	show := route["show"].(map[string]any)
	if show["tolls"] != true || show["polyline"] != false || show["freight_table"] != false || show["truck_scales"] != false ||
		route["config"].(map[string]any)["vehicle"].(map[string]any)["axis"] != float64(5) {
		t.Errorf("route request %v", route)
	}
	if fr := f.got["/tabela-frete/v1"]; fr["distance"] != 117.6 || fr["freight_type"] != "A" || fr["load_type"] != "conteineirizada" || fr["is_empty_return"] != false {
		t.Errorf("freight request %v", fr)
	}
	if got.DistanceKm != 117.6 || got.Freight != 1140.01 || got.LoadUnload != 497.66 || got.TollTotal != 228.5 || len(got.Tolls) != 2 ||
		got.Tolls[0].Name != "Riacho Grande" || got.Tolls[0].Amount != 179.5 || got.ResolutionURL != "https://www.in.gov.br/x" ||
		got.Resolution != "RESOLUÇÃO ANTT Nº 6.034" {
		t.Errorf("floor %+v", got)
	}
}

// TestFloorFailsClosed: an answer that's missing, zero, out of range, for
// another leg than asked, malformed or oversized is refused, never taken
// as a floor that anything passes.
func TestFloorFailsClosed(t *testing.T) {
	ctx := context.Background()
	manyTolls := `{"distancia":{"valor":10},"duracao":{"valor":60},"pedagios":[` +
		strings.TrimSuffix(strings.Repeat(`{"nome":"P","tarifa":{"5":1}},`, MaxTolls+1), ",") + `]}`
	for name, f := range map[string]*fake{
		"zero distance":       {route: strings.Replace(okRoute, `"valor":117.6`, `"valor":0`, 1), freight: okFreight},
		"negative distance":   {route: strings.Replace(okRoute, `"valor":117.6`, `"valor":-1`, 1), freight: okFreight},
		"a long drive":        {route: strings.Replace(okRoute, `"valor":7697`, `"valor":99999999`, 1), freight: okFreight},
		"too many tolls":      {route: manyTolls, freight: okFreight},
		"no fare for 5 axles": {route: strings.Replace(okRoute, `"tarifa":{"5":49}`, `"tarifa":{"6":49}`, 1), freight: okFreight},
		"zero floor":          {route: okRoute, freight: strings.Replace(okFreight, `"freight_cost":1140.01`, `"freight_cost":0`, 1)},
		"no data":             {route: okRoute, freight: `{"params":{"distance":117.6,"axis":5,"freight_type":"A","load_type":"conteineirizada"},"antt_resolution":{"name":"R"}}`},
		"loading over floor":  {route: okRoute, freight: strings.Replace(okFreight, `"load_unload_cost":497.66`, `"load_unload_cost":2000`, 1)},
		"huge floor":          {route: okRoute, freight: strings.Replace(okFreight, `1140.01`, `1e12`, 1)},
		"other axles":         {route: okRoute, freight: strings.Replace(okFreight, `"axis":5`, `"axis":6`, 1)},
		"other table":         {route: okRoute, freight: strings.Replace(okFreight, `"freight_type":"A"`, `"freight_type":"B"`, 1)},
		"other load":          {route: okRoute, freight: strings.Replace(okFreight, `"load_type":"conteineirizada"`, `"load_type":"geral"`, 1)},
		"other distance":      {route: okRoute, freight: strings.Replace(okFreight, `"distance":117.6`, `"distance":100`, 1)},
		"no resolution":       {route: okRoute, freight: strings.Replace(okFreight, `"name":"RESOLUÇÃO ANTT Nº 6.034"`, `"name":""`, 1)},
		"not JSON":            {route: `{`, freight: okFreight},
		"freight not JSON":    {route: okRoute, freight: `[`},
		"oversized":           {route: `{"x":"` + strings.Repeat("a", MaxBody) + `"}`, freight: okFreight},
		"a server error":      {status: http.StatusInternalServerError, body: "SECRET-BODY-TEXT"},
		"refused token":       {status: http.StatusUnauthorized},
	} {
		_, err := f.serve(t).Floor(ctx, leg)
		if err == nil {
			t.Errorf("%s: taken", name)
		} else if strings.Contains(err.Error(), "SECRET-BODY-TEXT") || strings.Contains(err.Error(), `"t"`) {
			t.Errorf("%s: the error carries QualP's text or the token: %v", name, err)
		}
	}
}

// TestUnsafeText: a toll's text with a hidden, reordering or line-breaking
// character, or too long, is refused, as an EDI partner's is.
func TestUnsafeText(t *testing.T) {
	for name, nome := range map[string]string{
		"bidi override":      "Riacho\u202e Grande",
		"variation selector": "Riacho\ufe0f Grande",
		"blank filler":       "Riacho\u3164Grande",
		"line break":         "Riacho\nGrande",
		"too long":           strings.Repeat("a", MaxText+1),
	} {
		raw, _ := json.Marshal(nome)
		f := &fake{route: strings.Replace(okRoute, `"Riacho Grande"`, string(raw), 1), freight: okFreight}
		if _, err := f.serve(t).Floor(context.Background(), leg); err == nil {
			t.Errorf("%s: taken", name)
		}
	}
}

// TestBeforeCalling: a leg QualP can't price (or wouldn't price as asked)
// is refused before any request, so no call is billed for it.
func TestBeforeCalling(t *testing.T) {
	f := &fake{route: okRoute, freight: okFreight}
	c := f.serve(t)
	for name, r := range map[string]backend.FloorRequest{
		"one place":    {Locations: []string{"Santos"}, Axles: 5, LoadType: "geral", Table: "A"},
		"one axle":     {Locations: leg.Locations, Axles: 1, LoadType: "geral", Table: "A"},
		"eight axles":  {Locations: leg.Locations, Axles: 8, LoadType: "geral", Table: "A"},
		"table E":      {Locations: leg.Locations, Axles: 5, LoadType: "geral", Table: "E"},
		"unknown load": {Locations: leg.Locations, Axles: 5, LoadType: "sand", Table: "A"},
	} {
		if _, err := c.Floor(context.Background(), r); err == nil {
			t.Errorf("%s: priced", name)
		}
	}
	if n := f.calls.Load(); n != 0 {
		t.Errorf("%d requests sent", n)
	}
}

// TestNoRedirects: a redirect is refused, not followed, so the token never
// reaches another host, whether or not the caller brings its own client.
func TestNoRedirects(t *testing.T) {
	var leaked atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Access-Token") != "" {
			leaked.Store(true)
		}
	}))
	t.Cleanup(other.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)
	for _, c := range []*Client{{BaseURL: srv.URL, Token: "t"}, {BaseURL: srv.URL, Token: "t", HTTP: &http.Client{}}} {
		if _, err := c.Floor(context.Background(), leg); err == nil {
			t.Error("a redirect followed")
		}
	}
	if leaked.Load() {
		t.Error("the token went to the redirect's host")
	}
}

// TestResolutionURL: only an https link to the gazette or the ANTT is
// kept; anything else is dropped, not shown as the official resolution.
func TestResolutionURL(t *testing.T) {
	for u, want := range map[string]string{
		"https://www.in.gov.br/x":                           "https://www.in.gov.br/x",
		"https://portal.antt.gov.br/r":                      "https://portal.antt.gov.br/r",
		"https://in.gov.br.evil.example/x":                  "",
		"https://evil.example/x":                            "",
		"http://www.in.gov.br/x":                            "",
		"javascript:alert(1)":                               "",
		"https://user@www.in.gov.br/x":                      "",
		`https://www.in.gov.br/"x y`:                        "",
		"https://www.in.gov.br/" + strings.Repeat("a", 600): "",
		"%zz": "",
	} {
		if got := officialURL(u); got != want {
			t.Errorf("officialURL(%q) = %q, want %q", u, got, want)
		}
	}
	if _, err := (&Client{BaseURL: "http://127.0.0.1:1", Token: "t"}).Floor(context.Background(), leg); err == nil {
		t.Error("an unreachable server answered")
	}
	if _, err := (&Client{BaseURL: "::", Token: "t"}).Floor(context.Background(), leg); err == nil {
		t.Error("a malformed URL answered")
	}
}
