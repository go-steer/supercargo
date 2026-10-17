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

package geo

import (
	"errors"
	"math"
	"testing"
)

func TestDecode(t *testing.T) {
	p, err := Decode("58V75885+8J")
	if err != nil || math.Abs(p.Lat+12.8342) > 0.001 || math.Abs(p.Lng+54.6909) > 0.001 {
		t.Fatalf("the farm: %v %v", p, err)
	}
	if s := p.String(); s != "-12.83419,-54.69094" {
		t.Errorf("String: %q", s)
	}
	if c := Encode(p); c != "58V75885+8J" {
		t.Errorf("Encode: %q", c)
	}
	for _, bad := range []string{"", "5885+8J", "58V7588", "58V75885+8I", "Z8V75885+8J"} {
		if _, err := Decode(bad); !errors.Is(err, ErrNotFull) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}
