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

// Package staticmap draws a leg on a map with Google's Maps Static API:
// its ends and corridor as markers, the corridor as a line. The node
// fetches the image itself, so the key never reaches a browser.
package staticmap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-steer/supercargo/internal/geo"
)

// Client is the Maps Static API. Its key goes in the URL (the API takes
// no header), so no error says the URL, and no redirect is followed.
type Client struct {
	Key string
	// BaseURL defaults to https://maps.googleapis.com.
	BaseURL string
	// Language is the map's labels' language (e.g. "pt-BR").
	Language string
	HTTP     *http.Client
}

// Map is what to draw: Pickup and Delivery (either may be nil), and the
// corridor's points between them.
type Map struct {
	Pickup, Delivery *geo.Point
	Via              []geo.Point
}

// Limits.
const (
	maxBody = 2 << 20
	maxVia  = 20
)

var (
	// ErrNothing is a map with fewer than two points.
	ErrNothing = errors.New("staticmap: fewer than two points to draw")
	// ErrFailed is a request that failed; why stays out (the URL holds
	// the key).
	ErrFailed = errors.New("staticmap: the request failed")
	pngMagic  = []byte("\x89PNG\r\n\x1a\n")
)

// Render is m as a PNG, 640×320 at scale 2.
func (c *Client) Render(ctx context.Context, m Map) ([]byte, error) {
	var pts []geo.Point
	if m.Pickup != nil {
		pts = append(pts, *m.Pickup)
	}
	pts = append(pts, m.Via[:min(len(m.Via), maxVia)]...)
	if m.Delivery != nil {
		pts = append(pts, *m.Delivery)
	}
	if len(pts) < 2 {
		return nil, ErrNothing
	}
	q := url.Values{"size": {"640x320"}, "scale": {"2"}, "maptype": {"roadmap"}}
	if m.Pickup != nil {
		q.Add("markers", "color:0x34A853|label:P|"+m.Pickup.String())
	}
	if len(m.Via) > 0 {
		var via []string
		for _, p := range m.Via[:min(len(m.Via), maxVia)] {
			via = append(via, p.String())
		}
		q.Add("markers", "size:small|color:0x5F6368|"+strings.Join(via, "|"))
	}
	if m.Delivery != nil {
		q.Add("markers", "color:0xEA4335|label:D|"+m.Delivery.String())
	}
	var path []string
	for _, p := range pts {
		path = append(path, p.String())
	}
	q.Set("path", "color:0x4285F4CC|weight:4|"+strings.Join(path, "|"))
	if c.Language != "" {
		q.Set("language", c.Language)
	}
	q.Set("key", c.Key)
	base := c.BaseURL
	if base == "" {
		base = "https://maps.googleapis.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(base, "/")+"/maps/api/staticmap?"+q.Encode(), nil)
	if err != nil {
		return nil, ErrFailed
	}
	client := http.Client{Timeout: 10 * time.Second}
	if c.HTTP != nil {
		client = *c.HTTP
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return nil, ErrFailed
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	switch {
	case err != nil:
		return nil, ErrFailed
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%w: HTTP %d", ErrFailed, resp.StatusCode)
	case len(body) > maxBody || !bytes.HasPrefix(body, pngMagic):
		return nil, fmt.Errorf("%w: not a PNG of at most %d bytes", ErrFailed, maxBody)
	}
	return body, nil
}
