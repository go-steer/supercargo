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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/go-steer/supercargo/pkg/intents"
)

// Place is a door or port a company ships from or to, as its own address
// book lists it: who is there (Party) and where (Place), in the form a
// door-to-door order carries them. A company's assistant drafts an order
// between two places by their IDs, so the addresses it sends are the
// company's own, never a model's.
type Place struct {
	ID    string        `json:"id"`
	Name  string        `json:"name"`
	Party intents.Party `json:"party"`
	Place intents.Place `json:"place"`
}

// MaxPlaces bounds an address book's places.
const MaxPlaces = 100

var placeID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// parsePlaces decodes places from their YAML form through JSON, so their
// fields are named as an order names them (un_location_code, …), strictly.
func parsePlaces(raw []map[string]any) ([]Place, error) {
	if len(raw) > MaxPlaces {
		return nil, fmt.Errorf("more than %d places", MaxPlaces)
	}
	out := make([]Place, 0, len(raw))
	seen := map[string]bool{}
	for i, m := range raw {
		b, err := json.Marshal(m)
		if err != nil {
			return nil, fmt.Errorf("places[%d]: %w", i, err)
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		var p Place
		if err := dec.Decode(&p); err != nil {
			return nil, fmt.Errorf("places[%d]: %w", i, err)
		}
		if err := p.Validate(); err != nil {
			return nil, fmt.Errorf("places[%d]: %w", i, err)
		}
		if seen[p.ID] {
			return nil, fmt.Errorf("places[%d]: %s appears twice", i, p.ID)
		}
		seen[p.ID] = true
		out = append(out, p)
	}
	return out, nil
}

// Validate checks a place: its ID and name, and its party and place as an
// order would carry them.
func (p *Place) Validate() error {
	if !placeID.MatchString(p.ID) {
		return fmt.Errorf("id %q is not 1-32 of [a-z0-9-]", p.ID)
	}
	if err := text("name", p.Name, MaxName); err != nil {
		return err
	}
	if p.Name == "" {
		return errors.New("name is empty")
	}
	// An order's dates are the order's, never the address book's.
	if p.Place.Earliest != "" || p.Place.Latest != "" {
		return fmt.Errorf("%s: a place carries no earliest or latest (an order sets its own)", p.ID)
	}
	// Held to an order's rules: the place as either end, the party as
	// either party.
	other := intents.Place{Type: intents.PlaceDoor, UNLocationCode: "BEANR", Address: &intents.Address{Street: "Probe 1", Locality: "Antwerp", Country: "BE"}}
	if p.Place.UNLocationCode == other.UNLocationCode {
		other.UNLocationCode, other.Address.Locality, other.Address.Country = "NLRTM", "Rotterdam", "NL"
	}
	probe := intents.ForwardingRequestBody{ShipperRef: "PROBE-1", Incoterms: intents.Incoterms{Rule: "DAP", Version: "2020", NamedPlace: p.Name}, // as an order names it
		Shipper: p.Party, Consignee: p.Party, PlaceOfReceipt: p.Place, PlaceOfDelivery: other,
		Equipment: []intents.Equipment{{ISOType: "45G1", Count: 1, GrossWeightKg: 1000}}, Cargo: intents.Cargo{Description: "probe", HSCode: "6404"}}
	if err := probe.Validate(); err != nil {
		return fmt.Errorf("%s: %w", p.ID, err)
	}
	return nil
}

// Places are the file's places (an address book's; a directory has none).
func (f *File) Places() []Place {
	if f == nil {
		return nil
	}
	return append([]Place(nil), f.places...)
}
