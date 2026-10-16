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

package llm

import (
	"strings"
	"testing"
)

// TestLanguageRule: English needs no rule; another language names itself
// and keeps what's exact, and what goes to another company, as it is.
func TestLanguageRule(t *testing.T) {
	for _, lang := range []string{"", "en", "xx"} {
		if r := LanguageRule(lang); r != "" {
			t.Errorf("%q: %q", lang, r)
		}
	}
	r := LanguageRule("pt-BR")
	for _, want := range []string{"Brazilian Portuguese", "exactly as they are", "another company in English"} {
		if !strings.Contains(r, want) {
			t.Errorf("the rule lacks %q: %s", want, r)
		}
	}
}
