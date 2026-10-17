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

// Package qualpmock is the demo's stand-in for QualP: an HTTP server that
// answers POST /rotas/v4 and POST /tabela-frete/v1 in QualP's own request
// and response shapes (API 4.0, as its OpenAPI document describes them), so
// the client in internal/qualp is the code that would call the real one.
// The routes are simulated: a small gazetteer of Brazilian places, road
// distance as the straight line times a detour factor, and toll plazas
// near the line, with plausible fares. The freight floor is computed with
// the ANTT's method from Resolução 6.034's coefficients (internal/antt),
// in force in 2024; the real service prices with the resolution in force
// now. Every route says it's simulated: roteador_selecionado is
// "simulado".
package qualpmock

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/go-steer/supercargo/internal/antt"
)

// Router is what a simulated route says chose it.
const Router = "simulado (supercargo qualpmock)"

// Server is the mock. Token, if set, is the only Access-Token accepted;
// otherwise any non-empty one is.
type Server struct {
	Token string

	mu  sync.Mutex
	seq int64
}

type place struct {
	name, uf string
	lat, lng float64
}

type plaza struct {
	id, name, operator, uf, city, road, km string
	lat, lng                               float64
	perAxle                                float64 // R$ per axle
}

// places are the gazetteer, by normalized key: the city's name, "city,
// uf", "city/uf", or its UN/LOCODE. A location may also be a point,
// "lat,lng" in decimal degrees (in Brazil), as QualP takes coordinates.
var places = map[string]place{}

func init() {
	for _, p := range []struct {
		place
		locode string
	}{
		{place{"Santos", "SP", -23.9608, -46.3336}, "BRSSZ"},
		{place{"Cajamar", "SP", -23.3561, -46.8769}, ""},
		{place{"São Paulo", "SP", -23.5505, -46.6333}, "BRSAO"},
		{place{"Campinas", "SP", -22.9056, -47.0608}, "BRCPQ"},
		{place{"Guarulhos", "SP", -23.4628, -46.5333}, "BRGRU"},
		{place{"Jundiaí", "SP", -23.1857, -46.8978}, ""},
		{place{"Sorocaba", "SP", -23.5015, -47.4526}, ""},
		{place{"São José dos Campos", "SP", -23.1896, -45.8841}, "BRSJK"},
		{place{"Ribeirão Preto", "SP", -21.1775, -47.8103}, "BRRAO"},
		{place{"Paranaguá", "PR", -25.5163, -48.5225}, "BRPNG"},
		{place{"Curitiba", "PR", -25.4284, -49.2733}, "BRCWB"},
		{place{"Ponta Grossa", "PR", -25.0950, -50.1619}, ""},
		{place{"Itajaí", "SC", -26.9078, -48.6619}, "BRITJ"},
		{place{"Joinville", "SC", -26.3045, -48.8487}, "BRJOI"},
		{place{"Rio de Janeiro", "RJ", -22.9068, -43.1729}, "BRRIO"},
		{place{"Belo Horizonte", "MG", -19.9167, -43.9345}, "BRBHZ"},
		{place{"Porto Alegre", "RS", -30.0346, -51.2177}, "BRPOA"},
		{place{"Rio Grande", "RS", -32.0350, -52.0986}, "BRRIG"},
		{place{"Vitória", "ES", -20.3155, -40.3128}, "BRVIX"},
		{place{"Brasília", "DF", -15.7975, -47.8919}, "BRBSB"},
		// Mato Grosso's grain and beef corridor (BR-163, BR-364).
		{place{"Sinop", "MT", -11.8642, -55.5066}, ""},
		{place{"Sorriso", "MT", -12.5425, -55.7211}, ""},
		{place{"Cuiabá", "MT", -15.6014, -56.0979}, "BRCGB"},
		{place{"Rondonópolis", "MT", -16.4673, -54.6372}, ""},
		{place{"Campo Grande", "MS", -20.4697, -54.6201}, "BRCGR"},
	} {
		for _, k := range []string{p.name, p.name + "," + p.uf, p.name + "/" + p.uf, p.locode} {
			if k != "" {
				places[key(k)] = p.place
			}
		}
	}
}

// plazas are toll plazas on São Paulo's and Paraná's main freight roads,
// with plausible per-axle truck fares (simulated, not a tariff table).
var plazas = []plaza{
	{"1001", "Riacho Grande", "Ecovias dos Imigrantes", "SP", "São Bernardo do Campo", "SP-150", "31.000", -23.7300, -46.5600, 35.90},
	{"1002", "Rodoanel Oeste", "CCR RodoAnel", "SP", "Barueri", "SP-021", "16.000", -23.5300, -46.8200, 2.70},
	{"1003", "Perus", "CCR AutoBAn", "SP", "São Paulo", "SP-330", "26.000", -23.4000, -46.8000, 9.80},
	{"1004", "Campo Limpo", "CCR AutoBAn", "SP", "Campo Limpo Paulista", "SP-348", "39.000", -23.2100, -46.8400, 10.10},
	{"1005", "Valinhos", "CCR AutoBAn", "SP", "Valinhos", "SP-330", "82.000", -22.9700, -47.0000, 9.60},
	// On the straight lines this mock routes by (its own simplification),
	// so the main corridors find their plazas.
	{"1764", "Arujá N/S", "CCR RioSP", "SP", "Arujá", "BR-116", "204.500", -23.4920, -46.3200, 4.50},
	{"1006", "Miracatu", "Arteris Régis Bittencourt", "SP", "Miracatu", "BR-116", "370.000", -24.0250, -47.3000, 3.10},
	{"1007", "Registro", "Arteris Régis Bittencourt", "SP", "Registro", "BR-116", "426.000", -24.3800, -47.8000, 3.10},
	{"1008", "São José dos Pinhais", "Ecovia Caminho do Mar", "PR", "São José dos Pinhais", "BR-277", "60.000", -25.4700, -48.9000, 26.40},
	// BR-163 in Mato Grosso, on the same straight lines.
	{"2001", "Sorriso", "Nova Rota do Oeste", "MT", "Sorriso", "BR-163", "742.000", -12.5370, -55.6130, 9.40},
	{"2002", "Nova Mutum", "Nova Rota do Oeste", "MT", "Nova Mutum", "BR-163", "593.000", -13.9200, -55.8300, 9.40},
	{"2003", "Jangada", "Nova Rota do Oeste", "MT", "Jangada", "BR-163", "460.000", -15.0400, -56.0100, 9.40},
	{"2004", "Campo Verde", "Nova Rota do Oeste", "MT", "Campo Verde", "BR-163", "197.000", -16.0340, -55.3680, 8.70},
}

// detour is how much longer a road is than the straight line, here: over
// a segment's first 300 km, and beyond (long hauls run on trunk roads).
const (
	detour     = 1.35
	detourLong = 1.15
)

// roadKm is a segment's road distance.
func roadKm(a, b place) float64 {
	d := haversine(a, b)
	if d <= 300 {
		return d * detour
	}
	return 300*detour + (d-300)*detourLong
}

// MaxBody bounds a request.
const MaxBody = 64 << 10

// Handler serves the mock's two endpoints.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /rotas/v4", s.auth(s.route))
	mux.HandleFunc("POST /tabela-frete/v1", s.auth(s.freight))
	return mux
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t := r.Header.Get("Access-Token")
		if t == "" || (s.Token != "" && subtle.ConstantTimeCompare([]byte(t), []byte(s.Token)) != 1) {
			answer(w, http.StatusUnauthorized, map[string]string{"message": "Unauthenticated."})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, MaxBody)
		next(w, r)
	}
}

// resolve finds each location, and the legs between them.
func resolve(locations []string) ([]place, float64, error) {
	if len(locations) < 2 || len(locations) > 25 {
		return nil, 0, fmt.Errorf("locations: 2 to 25 places")
	}
	var ps []place
	for _, l := range locations {
		p, ok := places[key(l)]
		if !ok {
			p, ok = point(l)
		}
		if !ok {
			return nil, 0, fmt.Errorf("location %q not found", trim(l))
		}
		ps = append(ps, p)
	}
	km := 0.0
	for i := 1; i < len(ps); i++ {
		km += roadKm(ps[i-1], ps[i])
	}
	return ps, math.Round(km*10) / 10, nil
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Locations []string `json:"locations"`
		Config    struct {
			Vehicle struct {
				Axis int `json:"axis"`
			} `json:"vehicle"`
		} `json:"config"`
		Show struct {
			Tolls bool `json:"tolls"`
		} `json:"show"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		answer(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid JSON body"})
		return
	}
	ps, km, err := resolve(req.Locations)
	if err != nil {
		answer(w, http.StatusUnprocessableEntity, map[string]string{"message": err.Error()})
		return
	}
	secs := int(math.Round(km / 55 * 3600)) // a loaded truck's average on these roads
	resp := map[string]any{
		"distancia":            map[string]any{"texto": fmt.Sprintf("%.0f km", km), "valor": km},
		"duracao":              map[string]any{"texto": fmt.Sprintf("%02d:%02d:%02d", secs/3600, secs%3600/60, secs%60), "valor": secs},
		"endereco_inicio":      strings.ToLower(ps[0].label()),
		"endereco_fim":         strings.ToLower(ps[len(ps)-1].label()),
		"coordenada_inicio":    fmt.Sprintf("%.5f,%.5f", ps[0].lat, ps[0].lng),
		"coordenada_fim":       fmt.Sprintf("%.5f,%.5f", ps[len(ps)-1].lat, ps[len(ps)-1].lng),
		"locais":               names(ps),
		"id_transacao":         s.next(),
		"roteador_selecionado": Router,
		"calcular_volta":       false, //nolint:misspell // QualP\'s field name ("calculate the return")
		"otimizar_rota":        false,
	}
	if req.Show.Tolls {
		resp["pedagios"] = tolls(ps)
	}
	answer(w, http.StatusOK, resp)
}

// tolls are the plazas within 8 km of a leg's straight line, in order
// along the route, with their fares for 2 to 9 axles.
func tolls(ps []place) []map[string]any {
	out := []map[string]any{}
	for i := 1; i < len(ps); i++ {
		type hit struct {
			p plaza
			t float64
		}
		var hits []hit
		for _, p := range plazas {
			if d, t := toSegment(ps[i-1], ps[i], p.lat, p.lng); d <= 8 && t >= 0 && t <= 1 {
				hits = append(hits, hit{p, t})
			}
		}
		slices.SortFunc(hits, func(a, b hit) int { return cmpFloat(a.t, b.t) })
		for _, h := range hits {
			tarifa := map[string]float64{}
			for ax := 2; ax <= 9; ax++ {
				tarifa[strconv.Itoa(ax)] = math.Round(h.p.perAxle*float64(ax)*100) / 100
			}
			out = append(out, map[string]any{"id_api": h.p.id, "nome": h.p.name, "concessionaria": h.p.operator, "uf": h.p.uf,
				"municipio": h.p.city, "rodovia": h.p.road, "km": h.p.km, "tarifa": tarifa})
		}
	}
	return out
}

func (s *Server) freight(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Distance      *float64 `json:"distance"`
		Locations     []string `json:"locations"`
		Axis          int      `json:"axis"`
		FreightType   string   `json:"freight_type"`
		LoadType      string   `json:"load_type"`
		IsEmptyReturn *bool    `json:"is_empty_return"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		answer(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid JSON body"})
		return
	}
	fail := func(msg string) { answer(w, http.StatusUnprocessableEntity, map[string]string{"message": msg}) }
	switch {
	case (req.Distance == nil) == (len(req.Locations) == 0):
		fail("send distance or locations, exclusively")
		return
	case req.IsEmptyReturn == nil:
		fail("is_empty_return is required")
		return
	case *req.IsEmptyReturn:
		fail("is_empty_return: the empty-return rule isn't simulated")
		return
	}
	km := 0.0
	if req.Distance != nil {
		km = *req.Distance
	} else {
		var err error
		if _, km, err = resolve(req.Locations); err != nil {
			fail(err.Error())
			return
		}
	}
	f, cc, err := antt.Floor(req.FreightType, req.LoadType, req.Axis, km)
	if err != nil {
		fail(strings.TrimPrefix(err.Error(), "antt: "))
		return
	}
	locations := req.Locations
	if locations == nil {
		locations = []string{}
	}
	params := map[string]any{"locations": locations, "distance": km, "axis": req.Axis, "freight_type": req.FreightType,
		"load_type": req.LoadType, "is_empty_return": false}
	answer(w, http.StatusOK, map[string]any{
		"params": params,
		"data":   map[string]float64{"freight_cost": f, "load_unload_cost": cc},
		"antt_resolution": map[string]string{"name": antt.Resolution.Name, "date": antt.Resolution.Date,
			"end_date": antt.Resolution.EndDate, "url": antt.Resolution.URL},
	})
}

func (s *Server) next() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	return 166944800 + s.seq
}

func answer(w http.ResponseWriter, status int, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		status, raw = http.StatusInternalServerError, []byte(`{"message":"internal error"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(raw, '\n'))
}

func names(ps []place) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.label()
	}
	return out
}

// label is "city/uf", or a point's coordinates.
func (p place) label() string {
	if p.uf == "" {
		return p.name
	}
	return p.name + "/" + p.uf
}

var coordinates = regexp.MustCompile(`^(-?[0-9]{1,2}(?:\.[0-9]{1,8})?),\s?(-?[0-9]{1,2}(?:\.[0-9]{1,8})?)$`)

// point is a location given as "lat,lng", if it's one in Brazil's box.
func point(l string) (place, bool) {
	m := coordinates.FindStringSubmatch(strings.TrimSpace(l))
	if m == nil {
		return place{}, false
	}
	lat, _ := strconv.ParseFloat(m[1], 64) // the pattern's digits parse
	lng, _ := strconv.ParseFloat(m[2], 64)
	if lat < -34 || lat > 6 || lng < -74 || lng > -34 {
		return place{}, false
	}
	return place{name: fmt.Sprintf("%.5f,%.5f", lat, lng), lat: lat, lng: lng}, true
}

// key normalizes a place's name: no accents, lower case, single spaces,
// none around a separator ("São Paulo / SP" is "sao paulo/sp").
func key(s string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(strings.ToLower(s)) {
		if !unicode.Is(unicode.Mn, r) {
			b.WriteRune(r)
		}
	}
	k := strings.Join(strings.Fields(b.String()), " ")
	return separators.ReplaceAllString(k, "$1")
}

var separators = regexp.MustCompile(` ?([,/]) ?`)

func trim(s string) string {
	if r := []rune(s); len(r) > 64 {
		return string(r[:64])
	}
	return s
}

const earthKm = 6371.0

func rad(d float64) float64 { return d * math.Pi / 180 }

func haversine(a, b place) float64 {
	dlat, dlng := rad(b.lat-a.lat), rad(b.lng-a.lng)
	h := math.Sin(dlat/2)*math.Sin(dlat/2) + math.Cos(rad(a.lat))*math.Cos(rad(b.lat))*math.Sin(dlng/2)*math.Sin(dlng/2)
	return 2 * earthKm * math.Asin(math.Sqrt(h))
}

// toSegment is a point's distance (km) from the segment a→b, on a local
// flat projection, and where along it the nearest point lies (0 at a, 1 at
// b).
func toSegment(a, b place, lat, lng float64) (km, t float64) {
	k := math.Cos(rad((a.lat + b.lat) / 2))
	ax, ay := a.lng*k, a.lat
	bx, by := b.lng*k, b.lat
	px, py := lng*k, lat
	dx, dy := bx-ax, by-ay
	l2 := dx*dx + dy*dy
	if l2 == 0 {
		return math.Hypot(px-ax, py-ay) * 111.32, 0
	}
	t = ((px-ax)*dx + (py-ay)*dy) / l2
	cx, cy := ax+t*dx, ay+t*dy
	return math.Hypot(px-cx, py-cy) * 111.32, t
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
