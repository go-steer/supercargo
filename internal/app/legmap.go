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
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/go-steer/supercargo/internal/config"
	"github.com/go-steer/supercargo/internal/control"
	"github.com/go-steer/supercargo/internal/geo"
	"github.com/go-steer/supercargo/internal/operator"
	"github.com/go-steer/supercargo/internal/staticmap"
	"github.com/go-steer/supercargo/internal/store"
)

// legMap draws the carrier's truck legs: the pickup and delivery by
// their Plus Codes, the corridor between. The control API serves the
// image (GET /bookings/{ref}/map) to the carrier's own people; each leg's
// is fetched once.
type legMap struct {
	render   func(context.Context, staticmap.Map) ([]byte, error)
	via      []geo.Point
	bookings *store.Bookings

	mu     sync.Mutex
	cache  map[string][]byte
	failed map[string]time.Time // when drawing a leg last failed
}

// maxMaps bounds the images (and failures) kept; failAgain is how long a
// failure stands before the API is asked again.
const (
	maxMaps   = 256
	failAgain = time.Minute
)

// evictOne makes room for one more entry in a full map (any one goes).
func evictOne[V any](m map[string]V) {
	if len(m) < maxMaps {
		return
	}
	for k := range m {
		delete(m, k)
		return
	}
}

func newLegMap(c *config.GoogleWeather, via []operator.Waypoint, bookings *store.Bookings) (*legMap, error) {
	if c == nil {
		return nil, nil
	}
	key := os.Getenv(c.APIKeyEnv)
	if key == "" {
		return nil, fmt.Errorf("road: planning: map: $%s is empty", c.APIKeyEnv)
	}
	m := &legMap{render: (&staticmap.Client{Key: key, Language: c.Language}).Render, bookings: bookings, cache: map[string][]byte{}, failed: map[string]time.Time{}}
	for _, w := range via {
		m.via = append(m.via, w.Point)
	}
	return m, nil
}

func (m *legMap) serve(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("ref")
	var leg *store.RoadBooking
	for _, b := range m.bookings.List() {
		if b.Ref == ref && b.Road != nil {
			leg = b.Road
		}
	}
	if leg == nil {
		control.Answer(w, http.StatusNotFound, errorJSON{Error: "no truck leg " + ref})
		return
	}
	m.mu.Lock()
	png, ok := m.cache[ref]
	failed, fresh := m.failed[ref]
	m.mu.Unlock()
	if fresh && time.Since(failed) < failAgain {
		// A failure is kept a while: a page's every refresh mustn't call
		// the API again.
		control.Answer(w, http.StatusBadGateway, errorJSON{Error: "the map isn't available"})
		return
	}
	if !ok {
		var draw staticmap.Map
		if p, err := geo.Decode(leg.Request.Pickup.PlusCode); err == nil {
			draw.Pickup = &p
		}
		if p, err := geo.Decode(leg.Request.Delivery.PlusCode); err == nil {
			draw.Delivery = &p
		}
		if draw.Pickup != nil && draw.Delivery != nil {
			draw.Via = m.via // the corridor runs between the two ends
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		var err error
		png, err = m.render(ctx, draw)
		switch {
		case errors.Is(err, staticmap.ErrNothing):
			control.Answer(w, http.StatusNotFound, errorJSON{Error: "this leg has no two points to draw"})
			return
		case err != nil:
			m.mu.Lock()
			evictOne(m.failed)
			m.failed[ref] = time.Now()
			m.mu.Unlock()
			control.Answer(w, http.StatusBadGateway, errorJSON{Error: err.Error()})
			return
		}
		m.mu.Lock()
		evictOne(m.cache)
		m.cache[ref] = png
		delete(m.failed, ref)
		m.mu.Unlock()
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(png)
}

type errorJSON struct {
	Error string `json:"error"`
}
