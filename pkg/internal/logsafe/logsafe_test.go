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
