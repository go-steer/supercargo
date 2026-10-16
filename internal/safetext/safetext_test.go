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

package safetext

import (
	"strings"
	"testing"
)

// TestProblem: real names in any script pass; anything that hides,
// reorders or breaks what's shown is named, never passed.
func TestProblem(t *testing.T) {
	for _, ok := range []string{"", "Riacho Grande", "São José dos Pinhais", "Rodoanel – Trecho Oeste (SP-021)", "東京港", "नमस\u094dत\u0947", "R$ 1.140,01"} {
		if p := Problem(ok, 64); p != "" {
			t.Errorf("Problem(%q) = %q", ok, p)
		}
	}
	for name, bad := range map[string]string{
		"invalid UTF-8":      "Riacho\xff",
		"too long":           strings.Repeat("a", 65),
		"line break":         "Riacho\nGrande",
		"tab":                "Riacho\tGrande",
		"bidi override":      "Riacho\u202e Grande",
		"zero width space":   "Riacho\u200bGrande",
		"variation selector": "Riacho\ufe0f",
		"blank filler":       "Riacho\u3164Grande",
		"braille blank":      "\u2800",
		"private use":        "\ue000",
		"unassigned":         "\U000e0080",
		"noncharacter":       "\ufdd0",
		"stacked marks":      "a" + strings.Repeat("\u0301", MaxMarks+1),
	} {
		if Problem(bad, 64) == "" {
			t.Errorf("%s: passed", name)
		}
	}
	if p := Problem("a"+strings.Repeat("\u0301", MaxMarks)+"b"+strings.Repeat("\u0301", MaxMarks), 64); p != "" {
		t.Errorf("marks counted across letters: %s", p)
	}
}
