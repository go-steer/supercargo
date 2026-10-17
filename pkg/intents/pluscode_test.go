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

package intents

import "testing"

// TestPlusCode: a stop or a place may be pinned by a full Plus Code, of
// ten or eleven digits; a short one, a padded one, lowercase or letters
// outside the code's alphabet are not one.
func TestPlusCode(t *testing.T) {
	stop := func(code string) error {
		s := Stop{Facility: "Fazenda Boa Safra", UNLocationCode: "BRCGB", PlusCode: code}
		return s.validate("pickup")
	}
	place := func(code string) error {
		p := Place{Type: PlaceDoor, UNLocationCode: "BRCGB", PlusCode: code, Address: &Address{Street: "MT-010", Locality: "Feliz Natal", Country: "BR"}}
		return p.validate("place")
	}
	for _, ok := range []string{"58V75885+8J", "58V75885+8JX", "", "CFX3X2W2+X2"} {
		if err := stop(ok); err != nil {
			t.Errorf("stop %q: %v", ok, err)
		}
		if err := place(ok); err != nil {
			t.Errorf("place %q: %v", ok, err)
		}
	}
	for _, bad := range []string{"5885+8J", "58V75885+", "58V75800+", "58v75885+8j", "58V75885+8A", "D8V75885+8J", "5WV75885+8J", "58V75885+8JXX", "58V75885 8J"} {
		if stop(bad) == nil || place(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
