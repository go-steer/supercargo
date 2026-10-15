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

package directory

import (
	"fmt"
	"strings"
	"testing"
)

const placesBook = `entries:
  - domain: meridian-forwarding.example
    name: Meridian Forwarding
    roles: [forwarder]
places:
  - id: tilburg-warehouse
    name: Tilburg warehouse
    party: { name: Cymbal Shoes B.V., identifiers: [{ scheme: EORI, value: NL123456789 }] }
    place: { type: door, un_location_code: NLTLB, address: { street: Ringbaan Oost 100, locality: Tilburg, country: NL } }
  - id: rotterdam-terminal
    name: Rotterdam
    party: { name: Cymbal Shoes B.V. }
    place: { type: port, facility: Euromax terminal, un_location_code: NLRTM }
`

// TestPlaces: an address book's places, as an order carries them; the
// forwarder role.
func TestPlaces(t *testing.T) {
	f, err := Parse([]byte(placesBook))
	if err != nil {
		t.Fatal(err)
	}
	ps := f.Places()
	if len(ps) != 2 || ps[0].ID != "tilburg-warehouse" || ps[0].Place.UNLocationCode != "NLTLB" || ps[0].Place.Address.Locality != "Tilburg" ||
		ps[0].Party.Identifiers[0].Value != "NL123456789" || ps[1].Place.Facility != "Euromax terminal" {
		t.Errorf("places %+v", ps)
	}
	ps[0].ID = "changed"
	if f.Places()[0].ID != "tilburg-warehouse" {
		t.Error("the caller changed the file's places")
	}
	if f.Entries()[0].Roles[0] != RoleForwarder {
		t.Error("no forwarder")
	}
	if (*File)(nil).Places() != nil {
		t.Error("a nil file has places")
	}
	// A place at Antwerp (where the check's other end would be) is fine.
	if _, err := Parse([]byte("places:\n  - { id: antwerp, name: A, party: { name: P }, place: { type: door, un_location_code: BEANR, address: { street: S 1, locality: Antwerp, country: BE } } }\n")); err != nil {
		t.Errorf("Antwerp: %v", err)
	}
}

// TestPlacesRefused: a place that isn't one an order could carry, or an
// address book that's not as specified, is refused.
func TestPlacesRefused(t *testing.T) {
	place := func(s string) string { return "places:\n  - " + s + "\n" }
	ok := `party: { name: P }, place: { type: door, un_location_code: NLTLB, address: { street: S 1, locality: T, country: NL } }`
	var many strings.Builder
	many.WriteString("places:\n")
	for i := range MaxPlaces + 1 {
		fmt.Fprintf(&many, "  - { id: p%d, name: P, %s }\n", i, ok)
	}
	for name, doc := range map[string]string{
		"bad id":                place(`{ id: "Tilburg Warehouse", name: T, ` + ok + ` }`),
		"no name":               place(`{ id: t, name: "", ` + ok + ` }`),
		"unknown field":         place(`{ id: t, name: T, colour: red, ` + ok + ` }`),
		"unknown inside":        place(`{ id: t, name: T, party: { name: P, phone: "1" }, place: { type: door, un_location_code: NLTLB, address: { street: S 1, locality: T, country: NL } } }`),
		"a door, no address":    place(`{ id: t, name: T, party: { name: P }, place: { type: door, un_location_code: NLTLB } }`),
		"a port, no terminal":   place(`{ id: t, name: T, party: { name: P }, place: { type: port, un_location_code: NLRTM } }`),
		"bad locode":            place(`{ id: t, name: T, party: { name: P }, place: { type: door, un_location_code: tilburg, address: { street: S 1, locality: T, country: NL } } }`),
		"no party name":         place(`{ id: t, name: T, party: { name: "" }, place: { type: door, un_location_code: NLTLB, address: { street: S 1, locality: T, country: NL } } }`),
		"twice":                 "places:\n  - { id: t, name: T, " + ok + " }\n  - { id: t, name: U, " + ok + " }\n",
		"too many":              many.String(),
		"bad role":              "entries:\n  - { domain: m.example, name: M, roles: [broker] }\n",
		"not JSON":              place(`{ id: t, name: T, weight: .nan, ` + ok + ` }`),
		"its own dates":         place(`{ id: t, name: T, party: { name: P }, place: { type: door, un_location_code: NLTLB, latest: "2026-11-07T12:00:00Z", address: { street: S 1, locality: T, country: NL } } }`),
		"a name no order takes": place("{ id: t, name: \"T\\uE000\", " + ok + " }"),
		"a control character":   place("{ id: t, name: \"T\\u0007\", " + ok + " }"),
	} {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
