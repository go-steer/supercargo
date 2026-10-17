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

// Package geo turns a place's Plus Code (an Open Location Code, the form
// a stop's plus_code carries) into the point at its centre, for the
// services that want coordinates: a route, the weather.
package geo

import (
	"errors"
	"fmt"
	"strconv"

	olc "github.com/google/open-location-code/go"
)

// Point is a latitude and longitude, in degrees.
type Point struct {
	Lat, Lng float64
}

// String is the point as "lat,lng", to five decimals (about a metre).
func (p Point) String() string {
	return strconv.FormatFloat(p.Lat, 'f', 5, 64) + "," + strconv.FormatFloat(p.Lng, 'f', 5, 64)
}

// ErrNotFull is a code that isn't a full Plus Code (a short one needs a
// reference place, which a stop doesn't carry).
var ErrNotFull = errors.New("geo: not a full Plus Code")

// Decode is the centre of the area a full Plus Code names.
func Decode(code string) (Point, error) {
	if err := olc.CheckFull(code); err != nil {
		return Point{}, fmt.Errorf("%w: %q", ErrNotFull, code)
	}
	area, err := olc.Decode(code)
	if err != nil {
		return Point{}, fmt.Errorf("geo: %w", err)
	}
	lat, lng := area.Center()
	return Point{lat, lng}, nil
}

// Encode is the 10-digit Plus Code of a point (an area about 14 m
// across).
func Encode(p Point) string {
	return olc.Encode(p.Lat, p.Lng, 10)
}
