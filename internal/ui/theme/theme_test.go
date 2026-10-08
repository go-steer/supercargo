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

package theme

import (
	"reflect"
	"strings"
	"testing"
)

func TestPalettesComplete(t *testing.T) {
	for _, p := range []Palette{Dark, Light} {
		v := reflect.ValueOf(p)
		for i := range v.NumField() {
			f := v.Type().Field(i)
			if f.Type.Kind() != reflect.String || f.Name == "Name" {
				continue
			}
			if _, _, _, err := parse(v.Field(i).String()); err != nil {
				t.Errorf("%s.%s: %v", p.Name, f.Name, err)
			}
		}
	}
	for _, c := range Aurora {
		if _, _, _, err := parse(c); err != nil {
			t.Error(err)
		}
	}
}

func TestByNameAndBackground(t *testing.T) {
	for name, want := range map[string]string{"dark": Dark.Name, "LIGHT": Light.Name, "grayscale-dark": Dark.Name} {
		if p, ok := ByName(name); !ok || p.Name != want {
			t.Errorf("%s: %s, %v", name, p.Name, ok)
		}
	}
	if _, ok := ByName("solarized"); ok {
		t.Error("an unknown theme")
	}
	for bg, want := range map[string]string{"#ffffff": Light.Name, "#f8f9fa": Light.Name, "#000000": Dark.Name,
		"#202124": Dark.Name, "nonsense": Dark.Name, "#zzzzzz": Dark.Name} {
		if got := ForBackground(bg).Name; got != want {
			t.Errorf("background %s: %s, want %s", bg, got, want)
		}
	}
}

func TestAurora(t *testing.T) {
	for at, want := range map[float64]string{0: "#217bfe", 1: "#ff5e62", -1: "#217bfe", 2: "#ff5e62", 1.0 / 3: "#078efb", 0.5: "#577ff5"} {
		if got := AuroraAt(at); got != want {
			t.Errorf("AuroraAt(%v) = %s, want %s", at, got, want)
		}
	}
	if Mix("bad", "#ffffff", 0.5) != "bad" || Mix("#000000", "#ffffff", 0.5) != "#808080" {
		t.Error("Mix")
	}
}

func TestCSS(t *testing.T) {
	css := Dark.CSS(`[data-theme="dark"]`)
	for _, want := range []string{`[data-theme="dark"] {`, "--bg: #202124;", "--green: #81c995;",
		"--aurora: linear-gradient(90deg, #217BFE, #078EFB, #A770EF, #FF5E62);", "--glow-border: rgba(167,112,239,.2);"} {
		if !strings.Contains(css, want) {
			t.Errorf("CSS lacks %q:\n%s", want, css)
		}
	}
}
