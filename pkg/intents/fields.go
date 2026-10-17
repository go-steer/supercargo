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

import (
	"fmt"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/internal/logsafe"
)

// Field formats. Bodies reach logs and model prompts, so identifiers stay
// short and inert, and free text has a length cap and no control
// characters.
var (
	// refPattern: no dots, so every reference a body carries can also be
	// asked about in a status request (statusRefPattern, the DCSA query's).
	refPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	// plusCodePattern is a full Open Location Code of ten or eleven
	// digits: latitude and longitude digit pairs from the code's alphabet
	// (the first latitude digit 0-8, the first longitude 0-17), the "+"
	// after eight, and two or three after it.
	plusCodePattern  = regexp.MustCompile(`^[2-9C][2-9CFGHJMPQRV][2-9CFGHJMPQRVWX]{6}\+[2-9CFGHJMPQRVWX]{2,3}$`)
	unLocodePattern  = regexp.MustCompile(`^[A-Z]{2}[A-Z2-9]{3}$`)
	isoTypePattern   = regexp.MustCompile(`^[0-9A-Z]{4}$`)
	hsCodePattern    = regexp.MustCompile(`^[0-9]{4,10}$`)
	containerPattern = regexp.MustCompile(`^[A-Z]{4}[0-9]{7}$`) // ISO 6346 form
	codePattern      = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
)

// DateLayout is the format of calendar dates in bodies.
const DateLayout = "2006-01-02"

func ref(field, s string) error {
	if !refPattern.MatchString(s) {
		return fmt.Errorf("%s: %s is not 1-64 characters of [A-Za-z0-9_-], starting with a letter or digit", field, logsafe.Quote(s))
	}
	return nil
}

func match(field, s string, re *regexp.Regexp) error {
	if !re.MatchString(s) {
		return fmt.Errorf("%s: %s does not match %s", field, logsafe.Quote(s), re)
	}
	return nil
}

// text checks a required free-text field: non-empty, at most max
// characters, no control characters. multiline allows '\n' and '\t'.
func text(field, s string, maxLen int, multiline bool) error {
	if s == "" {
		return fmt.Errorf("%s is required", field)
	}
	if n := utf8.RuneCountInString(s); n > maxLen {
		return fmt.Errorf("%s is %d characters, over the %d limit", field, n, maxLen)
	}
	for _, r := range s {
		if logsafe.Unsafe(r, multiline) {
			return fmt.Errorf("%s contains %U, which free text may not", field, r)
		}
	}
	return nil
}

func timestamp(field, s string) (time.Time, error) {
	t, err := time.Parse(envelope.TimeLayout, s)
	if err != nil || t.Format(envelope.TimeLayout) != s {
		return time.Time{}, fmt.Errorf("%s: %s is not RFC 3339 UTC in whole seconds", field, logsafe.Quote(s))
	}
	return t, nil
}

func date(field, s string) error {
	t, err := time.Parse(DateLayout, s)
	if err != nil || t.Format(DateLayout) != s {
		return fmt.Errorf("%s: %s is not a date (%s)", field, logsafe.Quote(s), DateLayout)
	}
	return nil
}
