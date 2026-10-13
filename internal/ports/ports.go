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

// Package ports names ports and container types for people. Bodies
// identify a port by its UN/LOCODE alone (the ULP payload specification's
// port_of_loading and port_of_discharge) and a container type by its ISO
// 6346 code; nodes show a name where they know one.
package ports

import "github.com/go-steer/supercargo/pkg/intents"

// names are the ports this demo knows by name.
var names = map[string]string{
	"NLRTM": "Rotterdam", "USEWR": "Newark", "USNYC": "New York", "DEHAM": "Hamburg", "BEANR": "Antwerp",
	"CNSHA": "Shanghai", "SGSIN": "Singapore", "GBFXT": "Felixstowe", "USLAX": "Los Angeles", "USLGB": "Long Beach",
	"CAHAL": "Halifax", "ESVLC": "Valencia", "FRLEH": "Le Havre", "JPTYO": "Tokyo", "KRPUS": "Busan",
	// The door-to-door journey's places: inland ones too, for its trucks.
	"BRSSZ": "Santos", "NLTLB": "Tilburg", "BRSAO": "São Paulo",
}

// coords are where the ports are, approximately (latitude, longitude in
// degrees): for drawing a journey (docs/UI route map), never for
// navigation. Like the names, they are this node's own, never a partner's.
var coords = map[string][2]float64{
	"NLRTM": {51.95, 4.05}, "USEWR": {40.68, -74.15}, "USNYC": {40.70, -74.01}, "DEHAM": {53.54, 9.97}, "BEANR": {51.26, 4.40},
	"CNSHA": {31.36, 121.60}, "SGSIN": {1.26, 103.84}, "GBFXT": {51.96, 1.33}, "USLAX": {33.74, -118.26}, "USLGB": {33.75, -118.21},
	"CAHAL": {44.64, -63.57}, "ESVLC": {39.44, -0.32}, "FRLEH": {49.48, 0.11}, "JPTYO": {35.62, 139.78}, "KRPUS": {35.10, 129.04},
	"BRSSZ": {-23.96, -46.30}, "NLTLB": {51.56, 5.09}, "BRSAO": {-23.55, -46.63},
}

// Coords is where the port is (latitude, longitude), if this node knows.
func Coords(code string) (lat, lng float64, ok bool) {
	c, ok := coords[code]
	return c[0], c[1], ok
}

// Name is the port's name, or its code if it has no known name.
// Names are the table's port names.
func Names() []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, n)
	}
	return out
}

func Name(code string) string {
	if n, ok := names[code]; ok {
		return n
	}
	return code
}

// Location is the port as a Location: its code and name.
func Location(code string) intents.Location {
	return intents.Location{UNLocode: code, Name: Name(code)}
}

// sizeLabels are the names people use for ISO 6346 size-types.
var sizeLabels = map[string]string{"22G1": "20GP", "42G1": "40GP", "45G1": "40HC", "L5G1": "45HC", "22R1": "20RF", "45R1": "40RF"}

// SizeLabel is the common name for an ISO 6346 size-type ("40HC" for
// 45G1), or the code itself.
func SizeLabel(iso string) string {
	if l, ok := sizeLabels[iso]; ok {
		return l
	}
	return iso
}

// SizeType is the ISO 6346 size-type for a common name ("45G1" for
// 40HC), or "".
func SizeType(label string) string {
	for iso, l := range sizeLabels {
		if l == label {
			return iso
		}
	}
	return ""
}
