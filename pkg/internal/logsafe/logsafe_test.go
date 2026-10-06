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

package logsafe

import (
	"strings"
	"testing"
)

// Runes are written as numbers: an invisible character in a source literal
// is exactly what this test is about.
func TestUnsafe(t *testing.T) {
	for _, r := range []rune{0x61, 0x5A, 0x30, 0x20, 0x2D, 0xE9, 0x6771, 0x1F600, 0x3000} {
		if Unsafe(r, false) {
			t.Errorf("Unsafe(%U) = true", r)
		}
	}
	for _, r := range []rune{
		0x00, 0x07, 0x0A, 0x09, 0x0D, 0x7F, 0x85, // control
		0x200B, 0x200D, 0x2060, 0xFEFF, 0xAD, // zero-width and format
		0x202E, 0x2066, // bidi
		0xE0001, 0xE0041, 0xE007F, // tag characters
		0xE000, 0xF0000, // private use
		0xFDD0, 0xFFFE, 0x1FFFF, // noncharacters
		0x2028, 0x2029, // separators
		0xFFFD, // replacement character
	} {
		if !Unsafe(r, false) {
			t.Errorf("Unsafe(%U) = false", r)
		}
	}
	if Unsafe(0x0A, true) || Unsafe(0x09, true) || !Unsafe(0x0D, true) {
		t.Error("multiline should allow exactly U+000A and U+0009")
	}
}

func TestQuote(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", `""`},
		{"kid-1", `"kid-1"`},
		{"a\nb", `"a\nb"`},
		{strings.Repeat("x", MaxLen), `"` + strings.Repeat("x", MaxLen) + `"`},
		{strings.Repeat("x", MaxLen+1), `"` + strings.Repeat("x", MaxLen) + `"...`},
		// A cut inside a multi-byte rune backs up to its start.
		{strings.Repeat("x", MaxLen-1) + "é", `"` + strings.Repeat("x", MaxLen-1) + `"...`},
	} {
		if got := Quote(tc.in); got != tc.want {
			t.Errorf("Quote(%.20q...) = %s, want %s", tc.in, got, tc.want)
		}
	}
	if got := Quote(strings.Repeat("y", 1<<20)); len(got) > MaxLen+8 {
		t.Errorf("Quote of a megabyte is %d bytes", len(got))
	}
}
