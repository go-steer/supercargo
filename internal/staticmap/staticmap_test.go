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

package staticmap

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-steer/supercargo/internal/geo"
)

var (
	farm = geo.Point{Lat: -12.83419, Lng: -54.69094}
	dc   = geo.Point{Lat: -23.46280, Lng: -46.53330}
)

func TestRender(t *testing.T) {
	var q []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q = append(q, r.URL.Path, r.URL.Query().Get("key"), r.URL.Query().Get("path"), strings.Join(r.URL.Query()["markers"], " "), r.URL.Query().Get("language"))
		_, _ = w.Write(append(pngMagic, "rest"...))
	}))
	defer s.Close()
	c := &Client{Key: "k3y", BaseURL: s.URL, Language: "pt-BR"}
	png, err := c.Render(context.Background(), Map{Pickup: &farm, Delivery: &dc, Via: []geo.Point{{Lat: -11.8642, Lng: -55.5066}}})
	if err != nil || !strings.HasSuffix(string(png), "rest") {
		t.Fatalf("%q %v", png, err)
	}
	want := []string{"/maps/api/staticmap", "k3y", "color:0x4285F4CC|weight:4|-12.83419,-54.69094|-11.86420,-55.50660|-23.46280,-46.53330",
		"color:0x34A853|label:P|-12.83419,-54.69094 size:small|color:0x5F6368|-11.86420,-55.50660 color:0xEA4335|label:D|-23.46280,-46.53330", "pt-BR"}
	if strings.Join(q, "\n") != strings.Join(want, "\n") {
		t.Errorf("asked\n%s", strings.Join(q, "\n"))
	}
}

// TestRenderRefuses: nothing to draw, an error, something not a PNG, a
// redirect, and no service each fail, and no error carries the key.
func TestRenderRefuses(t *testing.T) {
	if _, err := (&Client{}).Render(context.Background(), Map{Pickup: &farm}); !errors.Is(err, ErrNothing) {
		t.Errorf("one point: %v", err)
	}
	var redirected bool
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected = true }))
	defer other.Close()
	for name, h := range map[string]http.HandlerFunc{
		"an error":  func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "denied", http.StatusForbidden) },
		"not a PNG": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) },
		"too large": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(append(pngMagic, make([]byte, maxBody)...))
		},
		"a redirect": func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, http.StatusFound) },
	} {
		s := httptest.NewServer(h)
		_, err := (&Client{Key: "s3cret", BaseURL: s.URL}).Render(context.Background(), Map{Pickup: &farm, Delivery: &dc})
		if !errors.Is(err, ErrFailed) || strings.Contains(err.Error(), "s3cret") {
			t.Errorf("%s: %v", name, err)
		}
		s.Close()
	}
	if redirected {
		t.Error("followed a redirect")
	}
	if _, err := (&Client{Key: "s3cret", BaseURL: "http://127.0.0.1:1"}).Render(context.Background(), Map{Pickup: &farm, Delivery: &dc}); !errors.Is(err, ErrFailed) || strings.Contains(err.Error(), "s3cret") {
		t.Errorf("no service: %v", err)
	}
	if _, err := (&Client{Key: "s3cret", BaseURL: "http://bad host"}).Render(context.Background(), Map{Pickup: &farm, Delivery: &dc}); !errors.Is(err, ErrFailed) || strings.Contains(err.Error(), "s3cret") {
		t.Errorf("a bad URL: %v", err)
	}
}
