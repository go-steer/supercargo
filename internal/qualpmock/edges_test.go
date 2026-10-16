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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-steer/supercargo/internal/qualpmock"
)

func post(t *testing.T, url, path, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url+path, strings.NewReader(body))
	req.Header.Set("Access-Token", "any")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

// TestCorridors: the main corridors find their toll plazas.
func TestCorridors(t *testing.T) {
	srv := httptest.NewServer((&qualpmock.Server{}).Handler())
	t.Cleanup(srv.Close)
	for route, want := range map[string]string{
		`["São Paulo","Rio de Janeiro"]`: "Arujá N/S",
		`["São Paulo","Curitiba"]`:       "Miracatu,Registro",
		`["Curitiba","Paranaguá"]`:       "São José dos Pinhais",
	} {
		code, out := post(t, srv.URL, "/rotas/v4", `{"locations":`+route+`,"config":{"vehicle":{"axis":5}},"show":{"tolls":true}}`)
		var names []string
		for _, p := range out["pedagios"].([]any) {
			names = append(names, p.(map[string]any)["nome"].(string))
		}
		if code != 200 || strings.Join(names, ",") != want {
			t.Errorf("%s: HTTP %d, tolls %v, want %s", route, code, names, want)
		}
	}
	// Tolls only when asked.
	if _, out := post(t, srv.URL, "/rotas/v4", `{"locations":["Santos","Cajamar"]}`); out["pedagios"] != nil || out["roteador_selecionado"] != qualpmock.Router {
		t.Errorf("without show.tolls: %v", out)
	}
}

// TestLimits: too many places, an oversized body and a distance past the
// cap are refused with a proper error; a request by distance echoes no
// locations as an empty list, as QualP does.
func TestLimits(t *testing.T) {
	srv := httptest.NewServer((&qualpmock.Server{}).Handler())
	t.Cleanup(srv.Close)
	many := `["` + strings.TrimSuffix(strings.Repeat(`Santos","`, 26), `","`) + `"]`
	if code, _ := post(t, srv.URL, "/rotas/v4", `{"locations":`+many+`}`); code != 422 {
		t.Errorf("26 places: HTTP %d", code)
	}
	if code, _ := post(t, srv.URL, "/rotas/v4", `{"locations":["`+strings.Repeat("a", qualpmock.MaxBody)+`"]}`); code != 422 {
		t.Errorf("an oversized body: HTTP %d", code)
	}
	code, out := post(t, srv.URL, "/tabela-frete/v1", `{"distance":1e308,"axis":2,"freight_type":"A","load_type":"geral","is_empty_return":false}`)
	if code != 422 || out["message"] == nil {
		t.Errorf("a distance past the cap: HTTP %d %v", code, out)
	}
	_, out = post(t, srv.URL, "/tabela-frete/v1", `{"distance":100,"axis":2,"freight_type":"A","load_type":"granel_solido","is_empty_return":false}`)
	params := out["params"].(map[string]any)
	if l, ok := params["locations"].([]any); !ok || len(l) != 0 {
		t.Errorf("locations echoed as %v", params["locations"])
	}
	if out["data"].(map[string]any)["freight_cost"] != 740.59 {
		t.Errorf("QualP's documented example: %v", out["data"])
	}
}
