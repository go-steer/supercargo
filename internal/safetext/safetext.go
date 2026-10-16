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

// Package safetext is the rule for text a third party wrote that may reach
// a person's screen or a model: an EDI partner's names and error text, a
// routing service's toll plazas. Such text is valid UTF-8 of letters,
// marks, digits, punctuation, symbols and spaces: no controls, line
// breaks, bidirectional or other format characters, variation selectors,
// blank fillers, long stacks of combining marks, private-use or unassigned
// code points. It keeps text from hiding or reordering what's shown; it
// doesn't escape it: a renderer still escapes for its own medium (HTML,
// Markdown).
package safetext

import (
	"fmt"
	"unicode"
	"unicode/utf8"
)

// MaxMarks bounds combining marks in a row (enough for any script's real
// text, not for a stack meant to smear over its neighbours).
const MaxMarks = 4

// invisible are letters and symbols that show as blank.
var invisible = map[rune]bool{0x115F: true, 0x1160: true, 0x3164: true, 0xFFA0: true, 0x2800: true}

// Problem says what makes v unsafe, at most max characters long, or ""
// when it isn't.
func Problem(v string, max int) string {
	if !utf8.ValidString(v) {
		return "is not valid UTF-8"
	}
	if utf8.RuneCountInString(v) > max {
		return fmt.Sprintf("is over %d characters", max)
	}
	marks := 0
	for _, r := range v {
		if !unicode.In(r, unicode.L, unicode.M, unicode.N, unicode.P, unicode.S, unicode.Zs) ||
			unicode.In(r, unicode.Noncharacter_Code_Point, unicode.Variation_Selector) || invisible[r] {
			return fmt.Sprintf("has a control, format, invisible or unassigned character (%U)", r)
		}
		if unicode.Is(unicode.Mn, r) {
			if marks++; marks > MaxMarks {
				return fmt.Sprintf("has more than %d combining marks in a row", MaxMarks)
			}
		} else {
			marks = 0
		}
	}
	return ""
}
