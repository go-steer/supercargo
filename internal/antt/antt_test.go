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

package antt

import (
	"math"
	"testing"
)

// TestFloor: QualP's documented example (100 km, 2 axles, granel sólido,
// table A: R$ 740,59 with R$ 397,28 for loading) comes out exactly, and so
// does the demo's leg; what the resolution doesn't price is an error.
func TestFloor(t *testing.T) {
	for _, tc := range []struct {
		table, load string
		axles       int
		km          float64
		freight, cc float64
	}{
		{"A", "granel_solido", 2, 100, 740.59, 397.28},
		{"A", "conteineirizada", 5, 118, 1142.20, 497.66},
		{"B", "frigorificada", 5, 0, 516.81, 516.81},
		{"D", "geral", 9, 10, 0, 0}, // checked against the table below
	} {
		f, cc, err := Floor(tc.table, tc.load, tc.axles, tc.km)
		if err != nil {
			t.Fatalf("%+v: %v", tc, err)
		}
		if tc.freight == 0 {
			c := coefficients[tc.table][tc.load][tc.axles]
			tc.freight, tc.cc = cents(tc.km*c.ccd+c.cc), cents(c.cc)
		}
		if f != tc.freight || cc != tc.cc {
			t.Errorf("%+v: got %.2f, %.2f", tc, f, cc)
		}
	}
	for name, tc := range map[string]struct {
		table, load string
		axles       int
		km          float64
	}{
		"table E":               {"E", "geral", 5, 10},
		"unknown load":          {"A", "sand", 5, 10},
		"containers on 2 axles": {"A", "conteineirizada", 2, 10},
		"negative distance":     {"A", "geral", 5, -1},
		"not a number":          {"A", "geral", 5, math.NaN()},
		"infinite":              {"A", "geral", 5, math.Inf(1)},
	} {
		if _, _, err := Floor(tc.table, tc.load, tc.axles, tc.km); err == nil {
			t.Errorf("%s: priced", name)
		}
	}
	// Hand-copied from the gazette (Anexo II), independent of the table:
	// CCD × 100 km + CC.
	for _, tc := range []struct {
		table, load string
		axles       int
		ccd, cc     float64
	}{
		{"A", "perigosa_frigorificada", 7, 9.3406, 1079.10},
		{"A", "granel_pressurizada", 9, 8.4113, 897.78},
		{"B", "conteineirizada", 4, 4.4705, 448.18},
		{"C", "conteineirizada", 3, 3.6099, 161.73},
		// The gazette's anomalies, as published: not to be "fixed".
		{"C", "granel_solido", 6, 5.4526, 29.39},
		{"C", "frigorificada", 5, 4.7908, 235.81},
	} {
		f, cc, err := Floor(tc.table, tc.load, tc.axles, 100)
		if err != nil || f != cents(100*tc.ccd+tc.cc) || cc != tc.cc {
			t.Errorf("%+v: got %.2f, %.2f, %v", tc, f, cc, err)
		}
	}
	if _, _, err := Floor("A", "geral", 5, MaxKm+1); err == nil {
		t.Error("priced a distance over the cap")
	}
	// Every table prices every load type on some axles.
	for _, table := range Tables {
		for _, load := range LoadTypes {
			if len(coefficients[table][load]) == 0 {
				t.Errorf("table %s, %s: no coefficients", table, load)
			}
		}
	}
}
