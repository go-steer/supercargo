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

package weather

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/geo"
)

var now = time.Date(2026, 10, 20, 9, 30, 0, 0, time.UTC)

func hour(start time.Time, text string, deg float64, rain int) string {
	return fmt.Sprintf(`{"interval":{"startTime":%q,"endTime":%q},"weatherCondition":{"description":{"text":%q}},"temperature":{"unit":"CELSIUS","degrees":%v},"precipitation":{"probability":{"percent":%d}}}`,
		start.Format(time.RFC3339), start.Add(time.Hour).Format(time.RFC3339), text, deg, rain)
}

// fakeAPI answers like the Weather API: two pages of hours, and the
// current conditions. It records each request's key header and query.
func fakeAPI(t *testing.T, seen *[]string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, r.Header.Get("X-Goog-Api-Key")+" "+r.URL.Path+"?"+r.URL.RawQuery)
		switch r.URL.Path {
		case "/v1/currentConditions:lookup":
			_, _ = fmt.Fprint(w, `{"currentTime":"2026-10-20T09:30:00Z","weatherCondition":{"description":{"text":"Clear"}},"temperature":{"degrees":28.5},"precipitation":{"probability":{"percent":0}}}`)
		case "/v1/forecast/hours:lookup":
			start, next := now.Truncate(time.Hour), "p2"
			if r.URL.Query().Get("pageToken") == "p2" {
				start, next = start.Add(24*time.Hour), ""
			}
			var hs []string
			for i := range 24 {
				hs = append(hs, hour(start.Add(time.Duration(i)*time.Hour), fmt.Sprintf("Hour %d", i), 25, i))
			}
			_, _ = fmt.Fprintf(w, `{"forecastHours":[%s],"nextPageToken":%q}`, strings.Join(hs, ","), next)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func TestGoogle(t *testing.T) {
	var seen []string
	s := fakeAPI(t, &seen)
	g := &Google{Key: "k3y", BaseURL: s.URL, Language: "pt-BR", Now: func() time.Time { return now }}
	p := geo.Point{Lat: -11.86, Lng: -55.5}
	c, err := g.At(context.Background(), p, now.Add(30*time.Hour))
	if err != nil || c.Description != "Hour 6" || c.RainChance != 6 || !c.Forecast || c.Source != Source || !c.At.Equal(now.Truncate(time.Hour).Add(30*time.Hour)) {
		t.Fatalf("30 hours ahead: %+v %v", c, err)
	}
	if len(seen) != 2 || !strings.HasPrefix(seen[0], "k3y /v1/forecast/hours:lookup?") || strings.Contains(seen[0], "k3y?") || !strings.Contains(seen[0], "languageCode=pt-BR") || !strings.Contains(seen[1], "pageToken=p2") {
		t.Errorf("requests: %q", seen)
	}
	for _, at := range []time.Time{now.Add(-time.Hour), now.Add(300 * time.Hour)} {
		c, err := g.At(context.Background(), p, at)
		if err != nil || c.Description != "Clear" || c.TempC != 28.5 || c.Forecast {
			t.Errorf("%v: %+v %v", at, c, err)
		}
	}
}

// TestGoogleRefuses: what the API says is checked; nothing it sends is
// repeated in an error.
func TestGoogleRefuses(t *testing.T) {
	for name, body := range map[string]string{
		"unsafe text":     `{"weatherCondition":{"description":{"text":"Clear\u202e"}},"temperature":{"degrees":20}}`,
		"no temperature":  `{"weatherCondition":{"description":{"text":"Clear"}}}`,
		"fahrenheit":      `{"weatherCondition":{"description":{"text":"Clear"}},"temperature":{"unit":"FAHRENHEIT","degrees":80}}`,
		"too hot":         `{"weatherCondition":{"description":{"text":"Clear"}},"temperature":{"degrees":99}}`,
		"rain over 100%":  `{"weatherCondition":{"description":{"text":"Clear"}},"temperature":{"degrees":20},"precipitation":{"probability":{"percent":101}}}`,
		"not JSON":        `<html>`,
		"too large":       `{"x":"` + strings.Repeat("a", maxBody) + `"}`,
		"no description":  `{"temperature":{"degrees":20}}`,
		"a long one":      `{"weatherCondition":{"description":{"text":"` + strings.Repeat("a", 65) + `"}},"temperature":{"degrees":20}}`,
		"an error answer": "",
	} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if body == "" {
				http.Error(w, "secret detail", http.StatusForbidden)
				return
			}
			_, _ = fmt.Fprint(w, body)
		}))
		g := &Google{Key: "k", BaseURL: s.URL, Now: func() time.Time { return now }}
		c, err := g.At(context.Background(), geo.Point{Lat: 1, Lng: 1}, now.Add(-time.Hour))
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Errorf("%s: %+v %v", name, c, err)
		}
		s.Close()
	}
	g := &Google{Key: "k", BaseURL: "http://127.0.0.1:1", Now: func() time.Time { return now }}
	if _, err := g.At(context.Background(), geo.Point{Lat: 91}, now); err == nil {
		t.Error("a latitude beyond the pole")
	}
	if _, err := g.At(context.Background(), geo.Point{Lat: 1, Lng: 1}, now); err == nil {
		t.Error("no service")
	}
}

// TestGoogleNoHour: a forecast that never reaches the hour is an error,
// after a bounded number of pages.
func TestGoogleNoHour(t *testing.T) {
	pages := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		pages++
		_, _ = fmt.Fprintf(w, `{"forecastHours":[%s],"nextPageToken":"more"}`, hour(now.Add(-48*time.Hour), "Old", 20, 0))
	}))
	defer s.Close()
	g := &Google{Key: "k", BaseURL: s.URL, Now: func() time.Time { return now }}
	if _, err := g.At(context.Background(), geo.Point{Lat: 1, Lng: 1}, now.Add(5*time.Hour)); err == nil || pages != maxPages {
		t.Errorf("%v after %d pages", err, pages)
	}
}

func TestMock(t *testing.T) {
	m := &Mock{Source: "Weather (simulated)"}
	north, _ := m.At(context.Background(), geo.Point{Lat: -11.86, Lng: -55.5}, now)
	south, _ := m.At(context.Background(), geo.Point{Lat: -23.46, Lng: -46.53}, now)
	if north.RainChance != 70 || south.RainChance != 10 || north.Source != "Weather (simulated)" {
		t.Errorf("north %+v, south %+v", north, south)
	}
	if c, _ := m.At(context.Background(), geo.Point{}, time.Time{}); c.Forecast {
		t.Error("the current conditions: a forecast")
	}
}

// TestGoogleNoRedirect: a redirect isn't followed, so the key's header
// never reaches another host.
func TestGoogleNoRedirect(t *testing.T) {
	var leaked []string
	other := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		leaked = append(leaked, r.Header.Get("X-Goog-Api-Key"))
	}))
	defer other.Close()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusFound)
	}))
	defer s.Close()
	g := &Google{Key: "k3y", BaseURL: s.URL, HTTP: &http.Client{}, Now: func() time.Time { return now }}
	if _, err := g.At(context.Background(), geo.Point{Lat: 1, Lng: 1}, time.Time{}); err == nil || len(leaked) != 0 {
		t.Errorf("%v, the other host saw %q", err, leaked)
	}
}

func TestGoogleNegativeRain(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"weatherCondition":{"description":{"text":"Clear"}},"temperature":{"degrees":20},"precipitation":{"probability":{"percent":-1}}}`)
	}))
	defer s.Close()
	g := &Google{Key: "k", BaseURL: s.URL, Now: func() time.Time { return now }}
	if c, err := g.At(context.Background(), geo.Point{Lat: 1, Lng: 1}, time.Time{}); err == nil {
		t.Errorf("rain -1%%: %+v", c)
	}
}
