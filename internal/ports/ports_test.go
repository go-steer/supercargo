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

package ports

import (
	"testing"

	"github.com/go-steer/supercargo/pkg/intents"
)

func TestNames(t *testing.T) {
	if n := Name("NLRTM"); n != "Rotterdam" {
		t.Errorf("Name(NLRTM) = %q", n)
	}
	if n := Name("ZZZZZ"); n != "ZZZZZ" {
		t.Errorf("an unknown port is named %q, want its code", n)
	}
	if l := Location("USEWR"); l != (intents.Location{UNLocode: "USEWR", Name: "Newark"}) {
		t.Errorf("Location(USEWR) = %+v", l)
	}
}

func TestSizes(t *testing.T) {
	if l := SizeLabel("45G1"); l != "40HC" {
		t.Errorf("SizeLabel(45G1) = %q", l)
	}
	if l := SizeLabel("99X9"); l != "99X9" {
		t.Errorf("an unknown size-type is labeled %q, want its code", l)
	}
	if iso := SizeType("40HC"); iso != "45G1" {
		t.Errorf("SizeType(40HC) = %q", iso)
	}
	if iso := SizeType("53HC"); iso != "" {
		t.Errorf("SizeType(53HC) = %q, want none", iso)
	}
	for iso, l := range sizeLabels {
		if SizeType(l) != iso {
			t.Errorf("%s and %s don't round-trip", iso, l)
		}
	}
}

func TestCoords(t *testing.T) {
	if lat, lng, ok := Coords("NLRTM"); !ok || lat < 51 || lat > 52.5 || lng < 3.5 || lng > 4.6 {
		t.Errorf("Rotterdam at %v, %v (%v)", lat, lng, ok)
	}
	if _, _, ok := Coords("XXXXX"); ok {
		t.Error("an unknown port has coordinates")
	}
	for code := range names {
		if _, _, ok := Coords(code); !ok {
			t.Errorf("%s has a name and no coordinates", code)
		}
	}
}
