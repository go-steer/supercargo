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

// Package antt computes Brazil's minimum road freight floor (piso mínimo
// de frete, Lei nº 13.703/2018) from the ANTT's published coefficients:
// the floor for a leg is its distance times the displacement coefficient
// (CCD, R$/km) plus the loading and unloading coefficient (CC, R$), by
// table, load type and number of loaded axles. The coefficients are those
// of Resolução ANTT nº 6.034, in force from 18 January to 12 July 2024, as
// QualP's own documentation quotes them: not today's numbers (QualP prices
// with the resolution in force), but today's method. The demo's mock QualP
// computes its answers here. The table is generated from the gazette by
// dev/tools/gen-antt-coefficients, with its anomalies as published (table
// C, granel sólido, 6 axles: CC 29,39; table C, frigorificada, 5 axles:
// CCD 4,7908, out of step with its neighbours): pinned by a test, not
// "fixed" by hand.
package antt

import (
	"fmt"
	"math"
)

// Resolution is the resolution the coefficients come from.
var Resolution = struct{ Name, Date, EndDate, URL string }{
	Name:    "RESOLUÇÃO ANTT Nº 6.034",
	Date:    "18/01/2024",
	EndDate: "12/07/2024",
	URL:     "https://www.in.gov.br/en/web/dou/-/resolucao-n-6.034-de-18-de-janeiro-de-2024-538363035",
}

// Tables are the ANTT tables: A, the carrier's own tractor and trailer; B,
// the carrier's tractor only; C and D, the same in high-performance
// operations.
var Tables = []string{"A", "B", "C", "D"}

// LoadTypes are the load types, as QualP names them.
var LoadTypes = []string{"granel_solido", "granel_liquido", "frigorificada", "conteineirizada", "geral", "neogranel",
	"perigosa_granel_solido", "perigosa_granel_liquido", "perigosa_frigorificada", "perigosa_conteineirizada",
	"perigosa_geral", "granel_pressurizada"}

type coefficient struct{ ccd, cc float64 }

// Floor is the floor for km kilometres on table, for loadType and axles:
// the freight (km × CCD + CC) and the loading and unloading part (CC), in
// reais rounded to the centavo. An error names what isn't priced.
func Floor(table, loadType string, axles int, km float64) (freight, loadUnload float64, err error) {
	t, ok := coefficients[table]
	if !ok {
		return 0, 0, fmt.Errorf("antt: table %q is not A, B, C or D", table)
	}
	l, ok := t[loadType]
	if !ok {
		return 0, 0, fmt.Errorf("antt: load type %q is not one of the ANTT's", loadType)
	}
	c, ok := l[axles]
	if !ok {
		return 0, 0, fmt.Errorf("antt: table %s doesn't price %s on %d axles", table, loadType, axles)
	}
	if !(km >= 0) || km > MaxKm {
		return 0, 0, fmt.Errorf("antt: distance %v km is not 0 to %d", km, MaxKm)
	}
	return cents(km*c.ccd + c.cc), cents(c.cc), nil
}

// MaxKm bounds a leg's distance (Brazil is about 4,400 km across).
const MaxKm = 10_000

func cents(v float64) float64 { return math.Round(v*100) / 100 }
