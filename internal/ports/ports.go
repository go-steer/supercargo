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
}

// Name is the port's name, or its code if it has no known name.
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
