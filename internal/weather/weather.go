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

// Package weather is backend.Weather's adapters: Google's Weather API
// (Google Maps Platform), and the demo's mock.
package weather

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-steer/supercargo/internal/backend"
	"github.com/go-steer/supercargo/internal/geo"
	"github.com/go-steer/supercargo/internal/safetext"
)

// Google is Google's Weather API. The key goes in a header, so it is in
// no URL an error could carry.
type Google struct {
	Key string
	// BaseURL defaults to https://weather.googleapis.com.
	BaseURL string
	// Language is the descriptions' language (BCP 47, e.g. "pt-BR";
	// default English).
	Language string
	HTTP     *http.Client
	Now      func() time.Time
}

// Source is how the Google adapter's answers are labelled.
const Source = "Google Weather API"

// Limits on what the API answers.
const (
	maxBody  = 1 << 20
	maxPages = 10
	maxHours = 240 // the hourly forecast's reach
	maxText  = 64
)

type condition struct {
	WeatherCondition struct {
		Description struct {
			Text string `json:"text"`
		} `json:"description"`
	} `json:"weatherCondition"`
	Temperature struct {
		Degrees *float64 `json:"degrees"`
		Unit    string   `json:"unit"`
	} `json:"temperature"`
	Precipitation struct {
		Probability struct {
			Percent int `json:"percent"`
		} `json:"probability"`
	} `json:"precipitation"`
}

// At implements backend.Weather.
func (g *Google) At(ctx context.Context, p geo.Point, at time.Time) (*backend.Conditions, error) {
	if math.IsNaN(p.Lat) || math.Abs(p.Lat) > 90 || math.IsNaN(p.Lng) || math.Abs(p.Lng) > 180 {
		return nil, errors.New("weather: not a point")
	}
	now := time.Now
	if g.Now != nil {
		now = g.Now
	}
	q := url.Values{"location.latitude": {strconv.FormatFloat(p.Lat, 'f', 5, 64)}, "location.longitude": {strconv.FormatFloat(p.Lng, 'f', 5, 64)}}
	if g.Language != "" {
		q.Set("languageCode", g.Language)
	}
	ahead := at.Sub(now().Truncate(time.Hour))
	if ahead <= 0 || ahead >= maxHours*time.Hour {
		var cur struct {
			condition
			CurrentTime time.Time `json:"currentTime"`
		}
		if err := g.get(ctx, "/v1/currentConditions:lookup", q, &cur); err != nil {
			return nil, err
		}
		return g.conditions(cur.condition, cur.CurrentTime, false)
	}
	q.Set("hours", strconv.Itoa(int(ahead/time.Hour)+1))
	q.Set("pageSize", "24")
	for range maxPages {
		var page struct {
			ForecastHours []struct {
				condition
				Interval struct {
					StartTime time.Time `json:"startTime"`
					EndTime   time.Time `json:"endTime"`
				} `json:"interval"`
			} `json:"forecastHours"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := g.get(ctx, "/v1/forecast/hours:lookup", q, &page); err != nil {
			return nil, err
		}
		for _, h := range page.ForecastHours {
			if !at.Before(h.Interval.StartTime) && at.Before(h.Interval.EndTime) {
				return g.conditions(h.condition, h.Interval.StartTime, true)
			}
		}
		if page.NextPageToken == "" || len(page.NextPageToken) > 2048 {
			break
		}
		q.Set("pageToken", page.NextPageToken)
	}
	return nil, errors.New("weather: the forecast has no such hour")
}

// conditions checks what the API said before anyone sees it.
func (g *Google) conditions(c condition, at time.Time, forecast bool) (*backend.Conditions, error) {
	d, t, rain := c.WeatherCondition.Description.Text, c.Temperature.Degrees, c.Precipitation.Probability.Percent
	switch {
	case d == "" || safetext.Problem(d, maxText) != "":
		return nil, errors.New("weather: the description isn't safe to show")
	case t == nil || math.IsNaN(*t) || *t < -90 || *t > 70 || (c.Temperature.Unit != "" && c.Temperature.Unit != "CELSIUS"):
		return nil, errors.New("weather: the temperature is missing or out of range")
	case rain < 0 || rain > 100:
		return nil, errors.New("weather: the chance of rain is out of range")
	}
	return &backend.Conditions{Description: d, TempC: *t, RainChance: rain, At: at.UTC(), Forecast: forecast, Source: Source}, nil
}

func (g *Google) get(ctx context.Context, path string, q url.Values, out any) error {
	base := g.BaseURL
	if base == "" {
		base = "https://weather.googleapis.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(base, "/")+path+"?"+q.Encode(), nil)
	if err != nil {
		return fmt.Errorf("weather: %w", err)
	}
	req.Header.Set("X-Goog-Api-Key", g.Key)
	client := g.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	// Never a redirect: the key's header would go with it to the other
	// host (net/http keeps every header but Authorization and cookies).
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return errRedirect }
	client = &c
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("weather: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	switch {
	case err != nil:
		return fmt.Errorf("weather: %w", err)
	case len(body) > maxBody:
		return errors.New("weather: the answer is too large")
	case resp.StatusCode != http.StatusOK:
		// The body isn't repeated: it is the service's text, not ours.
		return fmt.Errorf("weather: HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("weather: %w", err)
	}
	return nil
}

// Mock is the demo's weather, without a key: the rainy season's
// thunderstorms north of 14°S (Mato Grosso's north in October), fair
// weather elsewhere. Source labels it, e.g. "Weather (simulated)".
type Mock struct {
	Source string
}

// errRedirect refuses a redirect.
var errRedirect = errors.New("weather: the service redirected; not followed")

// At implements backend.Weather: a zero at is the current conditions.
func (m *Mock) At(_ context.Context, p geo.Point, at time.Time) (*backend.Conditions, error) {
	c := &backend.Conditions{Description: "Partly cloudy", TempC: 27, RainChance: 10, At: at.UTC().Truncate(time.Hour), Forecast: !at.IsZero(), Source: m.Source}
	if p.Lat > -14 && p.Lat < 0 {
		c.Description, c.TempC, c.RainChance = "Thunderstorms", 31, 70
	}
	return c, nil
}
