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

// Package logsafe renders untrusted strings for error messages, which end up
// in logs: quoted, so they cannot forge log lines, and truncated, so a
// megabyte of attacker input does not become a megabyte of log.
package logsafe

import (
	"strconv"
	"unicode/utf8"
)

// MaxLen is how many bytes of an untrusted string Quote keeps.
const MaxLen = 64

// Quote returns s as a Go-quoted string, cut to MaxLen bytes (on a rune
// boundary) with "..." appended when it was longer.
func Quote(s string) string {
	if len(s) <= MaxLen {
		return strconv.Quote(s)
	}
	cut := MaxLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strconv.Quote(s[:cut]) + "..."
}
