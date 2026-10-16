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

// Package qualp is a client of QualP (api.qualp.com.br, API 4.0), a
// Brazilian routing service: a road leg's distance and toll plazas
// (POST /rotas/v4), and the ANTT minimum freight floor for that distance
// (POST /tabela-frete/v1), as its OpenAPI document describes them. It
// implements backend.FreightFloors. What QualP answers is a third party's
// data: it is checked before use (sizes, numbers in range, the inputs it
// echoes, text by internal/safetext's rule), since toll names may reach a
// person's screen or a model. A missing or zero floor is an error, never a
// floor anything passes: the check fails closed.
package qualp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-steer/supercargo/internal/antt"
	"github.com/go-steer/supercargo/internal/backend"
	"github.com/go-steer/supercargo/internal/safetext"
)

// The request and response bodies, as QualP's OpenAPI document names their
// members (only those used here; others are ignored).
type (
	// RouteRequest is POST /rotas/v4's body.
	RouteRequest struct {
		Locations []string    `json:"locations"`
		Config    RouteConfig `json:"config"`
		Show      RouteShow   `json:"show"`
	}
	RouteConfig struct {
		Route   RouteOptions  `json:"route"`
		Vehicle VehicleConfig `json:"vehicle"`
	}
	RouteOptions struct {
		TypeRoute string `json:"type_route"`
	}
	VehicleConfig struct {
		Type string `json:"type"`
		Axis int    `json:"axis"`
	}
	// RouteShow says what to return: the tolls, and none of the heavy
	// parts the API returns by default (polylines, the QualP site link,
	// truck scales, every freight table).
	RouteShow struct {
		Tolls              bool `json:"tolls"`
		Polyline           bool `json:"polyline"`
		SimplifiedPolyline bool `json:"simplified_polyline"`
		LinkToQualP        bool `json:"link_to_qualp"`
		TruckScales        bool `json:"truck_scales"`
		FreightTable       bool `json:"freight_table"`
	}
	// RouteResponse is POST /rotas/v4's answer.
	RouteResponse struct {
		Distancia           Measure   `json:"distancia"`
		Duracao             Measure   `json:"duracao"`
		EnderecoInicio      string    `json:"endereco_inicio"`
		EnderecoFim         string    `json:"endereco_fim"`
		Pedagios            []Pedagio `json:"pedagios"`
		Locais              []string  `json:"locais"`
		IDTransacao         int64     `json:"id_transacao"`
		RoteadorSelecionado string    `json:"roteador_selecionado"`
	}
	Measure struct {
		Texto string  `json:"texto"`
		Valor float64 `json:"valor"`
	}
	// Pedagio is a toll plaza on the route; Tarifa is by number of axles.
	Pedagio struct {
		IDAPI          string             `json:"id_api"`
		Nome           string             `json:"nome"`
		Concessionaria string             `json:"concessionaria"`
		UF             string             `json:"uf"`
		Municipio      string             `json:"municipio"`
		Rodovia        string             `json:"rodovia"`
		KM             string             `json:"km"`
		Tarifa         map[string]float64 `json:"tarifa"`
	}
	// FreightRequest is POST /tabela-frete/v1's body: by distance here.
	FreightRequest struct {
		Distance      float64 `json:"distance"`
		Axis          int     `json:"axis"`
		FreightType   string  `json:"freight_type"`
		LoadType      string  `json:"load_type"`
		IsEmptyReturn bool    `json:"is_empty_return"`
	}
	// FreightResponse is POST /tabela-frete/v1's answer.
	FreightResponse struct {
		Params struct {
			Distance    float64 `json:"distance"`
			Axis        int     `json:"axis"`
			FreightType string  `json:"freight_type"`
			LoadType    string  `json:"load_type"`
		} `json:"params"`
		Data struct {
			FreightCost    float64 `json:"freight_cost"`
			LoadUnloadCost float64 `json:"load_unload_cost"`
		} `json:"data"`
		AnttResolution struct {
			Name    string `json:"name"`
			Date    string `json:"date"`
			EndDate string `json:"end_date"`
			URL     string `json:"url"`
		} `json:"antt_resolution"`
	}
)

// Client calls QualP at BaseURL (https://api.qualp.com.br, or the demo's
// mock) with Token as its Access-Token. Source names it in what it
// answers ("qualp", or "simulated qualp" for the mock).
type Client struct {
	BaseURL string
	Token   string
	Source  string
	HTTP    *http.Client // default: a 15 s timeout
}

// Limits on what QualP answers.
const (
	MaxBody     = 1 << 20 // bytes
	MaxTolls    = 100
	MaxDistance = 10_000 // km
	MaxAmount   = 1e7    // R$
	MaxText     = 128    // characters of a name
)

// ErrUnauthorized is a token QualP refuses.
var ErrUnauthorized = errors.New("qualp: the access token was refused")

// Floor implements backend.FreightFloors: the route's distance and tolls
// for the vehicle, then the ANTT floor for that distance.
func (c *Client) Floor(ctx context.Context, r backend.FloorRequest) (*backend.Floor, error) {
	if len(r.Locations) < 2 || r.Axles < 2 || r.Axles > 9 || r.Axles == 8 {
		return nil, errors.New("qualp: a leg has at least two places and 2 to 7 or 9 axles")
	}
	if !slices.Contains(antt.Tables, r.Table) || !slices.Contains(antt.LoadTypes, r.LoadType) {
		return nil, fmt.Errorf("qualp: table %q or load type %q isn't the ANTT's", r.Table, r.LoadType)
	}
	var route RouteResponse
	if err := c.post(ctx, "/rotas/v4", RouteRequest{Locations: r.Locations,
		Config: RouteConfig{Route: RouteOptions{TypeRoute: "efficient"}, Vehicle: VehicleConfig{Type: "truck", Axis: r.Axles}},
		Show:   RouteShow{Tolls: true}}, &route); err != nil {
		return nil, err
	}
	km, secs := route.Distancia.Valor, route.Duracao.Valor
	if !inRange(km, MaxDistance) || km == 0 || !inRange(secs, 30*24*3600) || len(route.Pedagios) > MaxTolls {
		return nil, errors.New("qualp: the route's distance, duration or tolls are out of range")
	}
	out := &backend.Floor{DistanceKm: km, Duration: time.Duration(secs) * time.Second, Source: c.source()}
	axles := strconv.Itoa(r.Axles)
	for n, p := range route.Pedagios {
		amount, ok := p.Tarifa[axles]
		if !ok || !inRange(amount, MaxAmount) {
			return nil, fmt.Errorf("qualp: toll plaza %d has no fare for %d axles", n+1, r.Axles)
		}
		for _, v := range []string{p.Nome, p.Concessionaria, p.Rodovia, p.UF} {
			if safetext.Problem(v, MaxText) != "" {
				return nil, fmt.Errorf("qualp: toll plaza %d's text isn't safe to show", n+1)
			}
		}
		out.Tolls = append(out.Tolls, backend.Toll{Name: p.Nome, Operator: p.Concessionaria, Road: p.Rodovia, State: p.UF, Amount: amount})
		out.TollTotal += amount
	}
	out.TollTotal = math.Round(out.TollTotal*100) / 100
	var freight FreightResponse
	if err := c.post(ctx, "/tabela-frete/v1", FreightRequest{Distance: km, Axis: r.Axles, FreightType: r.Table, LoadType: r.LoadType}, &freight); err != nil {
		return nil, err
	}
	d, e := freight.Data, freight.Params
	switch {
	case !inRange(d.FreightCost, MaxAmount) || !inRange(d.LoadUnloadCost, MaxAmount) || d.LoadUnloadCost <= 0 || d.FreightCost < d.LoadUnloadCost:
		return nil, errors.New("qualp: the freight floor is missing or out of range")
	case e.Axis != r.Axles || e.FreightType != r.Table || e.LoadType != r.LoadType || math.Abs(e.Distance-km) > 0.5:
		// Priced for something else (a default the server applied).
		return nil, errors.New("qualp: the freight floor is for another leg than asked")
	}
	name := freight.AnttResolution.Name
	if name == "" || safetext.Problem(name, MaxText) != "" {
		return nil, errors.New("qualp: the floor names no ANTT resolution")
	}
	out.Freight, out.LoadUnload, out.Resolution = d.FreightCost, d.LoadUnloadCost, name
	out.ResolutionURL = officialURL(freight.AnttResolution.URL)
	return out, nil
}

// officialURL is u if it's an https link to the official gazette or the
// ANTT (the resolution is presented as official), else "".
func officialURL(u string) string {
	p, err := url.Parse(u)
	if err != nil || len(u) > 512 || p.Scheme != "https" || p.User != nil || strings.ContainsAny(u, " \"'<>") {
		return ""
	}
	for _, host := range []string{"in.gov.br", "antt.gov.br"} {
		if h := p.Hostname(); h == host || strings.HasSuffix(h, "."+host) {
			return u
		}
	}
	return ""
}

func (c *Client) source() string {
	if c.Source == "" {
		return "qualp"
	}
	return c.Source
}

// post sends body to path and decodes the answer into out.
func (c *Client) post(ctx context.Context, path string, body, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(c.BaseURL, "/")+path, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("qualp: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Access-Token", c.Token)
	hc := &http.Client{Timeout: 15 * time.Second}
	if c.HTTP != nil {
		copied := *c.HTTP
		hc = &copied
	}
	// A redirect is refused, not followed: Go would send Access-Token, a
	// custom header, on to wherever it points.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("qualp: %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody+1))
	if err != nil {
		return fmt.Errorf("qualp: %s: %w", path, err)
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return ErrUnauthorized
	case resp.StatusCode != http.StatusOK:
		// The status only: the body is QualP's text, not ours to show.
		return fmt.Errorf("qualp: %s answered %d", path, resp.StatusCode)
	case len(data) > MaxBody:
		return fmt.Errorf("qualp: %s answered over %d bytes", path, MaxBody)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("qualp: %s: %w", path, err)
	}
	return nil
}

func inRange(v, limit float64) bool { return v >= 0 && v <= limit && !math.IsNaN(v) }
