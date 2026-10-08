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

// Package theme holds the UI's colour tokens: the one source for the
// TUI, the pretty node logs and the browser UI (which gets them as CSS
// custom properties).
//
// The palettes are mast-web's Google grayscale themes (grayscale-dark and
// grayscale-light). The Gemini aurora is the accent for model output
// only: the wordmark, assistant rows, alert summaries and model-call log
// lines.
//
// Colour means the same everywhere: green is verified, allowed, held
// signed; red is rejected, denied; yellow is pending, a high alert; blue
// is links and focus.
package theme

import (
	hexpkg "encoding/hex"
	"fmt"
	"math"
	"strings"
)

// Palette is one theme's colours, as #rrggbb.
type Palette struct {
	Name       string
	Dark       bool
	Bg         string // the page
	Surface    string // panels
	Elevated   string // dialogs, the selected row
	Border     string
	BorderSoft string
	Text       string
	Dim        string // labels, timestamps, hints
	Green      string // verified, allowed, held signed
	Red        string // rejected, denied
	Yellow     string // pending, high alert
	Blue       string // links, focus
}

// The palettes, from mast-web's web/styles.css.
var (
	Dark = Palette{Name: "grayscale-dark", Dark: true,
		Bg: "#202124", Surface: "#1a1b1e", Elevated: "#2a2b2e", Border: "#3c4043", BorderSoft: "#5f6368",
		Text: "#e8eaed", Dim: "#9aa0a6", Green: "#81c995", Red: "#f28b82", Yellow: "#fdd663", Blue: "#8ab4f8"}
	Light = Palette{Name: "grayscale-light",
		Bg: "#f8f9fa", Surface: "#ffffff", Elevated: "#f1f3f4", Border: "#dadce0", BorderSoft: "#bdc1c6",
		Text: "#202124", Dim: "#5f6368", Green: "#188038", Red: "#d50000", Yellow: "#dc6d00", Blue: "#1a73e8"}
)

// Aurora is the Gemini gradient's stops, left to right.
var Aurora = []string{"#217BFE", "#078EFB", "#A770EF", "#FF5E62"}

// Glow is the browser UI's card glow for model output, with its border.
const (
	GlowShadow = "0 0 20px rgba(33,123,254,.15), 0 0 40px rgba(167,112,239,.1)"
	GlowBorder = "rgba(167,112,239,.2)"
)

// ByName returns the palette for "dark" or "light" (or its full name).
func ByName(name string) (Palette, bool) {
	switch strings.ToLower(name) {
	case "dark", Dark.Name:
		return Dark, true
	case "light", Light.Name:
		return Light, true
	}
	return Palette{}, false
}

// ForBackground picks the palette for a terminal whose background is
// hex (#rrggbb): light on a light background, dark otherwise.
func ForBackground(hex string) Palette {
	r, g, b, err := parse(hex)
	if err == nil && luminance(r, g, b) > 0.5 {
		return Light
	}
	return Dark
}

// AuroraAt is the gradient's colour at t in [0, 1].
func AuroraAt(t float64) string {
	t = math.Max(0, math.Min(1, t))
	if t == 1 {
		return strings.ToLower(Aurora[len(Aurora)-1])
	}
	seg := t * float64(len(Aurora)-1)
	i := int(seg)
	return Mix(Aurora[i], Aurora[i+1], seg-float64(i))
}

// Mix blends a toward b by t in [0, 1]. A colour that doesn't parse is
// returned as given.
func Mix(a, b string, t float64) string {
	ar, ag, ab, err1 := parse(a)
	br, bg, bb, err2 := parse(b)
	if err1 != nil || err2 != nil {
		return a
	}
	mix := func(x, y uint8) uint8 { return uint8(math.Round(float64(x) + (float64(y)-float64(x))*t)) }
	return fmt.Sprintf("#%02x%02x%02x", mix(ar, br), mix(ag, bg), mix(ab, bb))
}

// CSS returns the palette and the aurora as CSS custom properties for
// selector, e.g. `[data-theme="dark"]`.
func (p Palette) CSS(selector string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s {\n", selector)
	for _, v := range []struct{ name, value string }{
		{"bg", p.Bg}, {"surface", p.Surface}, {"elevated", p.Elevated}, {"border", p.Border},
		{"border-soft", p.BorderSoft}, {"text", p.Text}, {"dim", p.Dim}, {"green", p.Green},
		{"red", p.Red}, {"yellow", p.Yellow}, {"blue", p.Blue},
	} {
		fmt.Fprintf(&b, "  --%s: %s;\n", v.name, v.value)
	}
	fmt.Fprintf(&b, "  --aurora: linear-gradient(90deg, %s);\n", strings.Join(Aurora, ", "))
	fmt.Fprintf(&b, "  --glow-shadow: %s;\n  --glow-border: %s;\n", GlowShadow, GlowBorder)
	b.WriteString("}\n")
	return b.String()
}

func parse(hex string) (r, g, b uint8, err error) {
	if len(hex) != 7 || hex[0] != '#' {
		return 0, 0, 0, fmt.Errorf("theme: %q is not #rrggbb", hex)
	}
	v, err := hexpkg.DecodeString(hex[1:])
	if err != nil {
		return 0, 0, 0, fmt.Errorf("theme: %q is not #rrggbb", hex)
	}
	return v[0], v[1], v[2], nil
}

// luminance is the relative luminance (WCAG) of an sRGB colour.
func luminance(r, g, b uint8) float64 {
	lin := func(c uint8) float64 {
		s := float64(c) / 255
		if s <= 0.03928 {
			return s / 12.92
		}
		return math.Pow((s+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(r) + 0.7152*lin(g) + 0.0722*lin(b)
}
