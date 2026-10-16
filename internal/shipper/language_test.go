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

package shipper

import (
	"context"
	"strings"
	"testing"
)

// TestSummaryLanguage: the notification is asked for in the node's
// language; English needs no rule.
func TestSummaryLanguage(t *testing.T) {
	for lang, want := range map[string]bool{"pt-BR": true, "": false} {
		m := FakeSummaryModel()
		alert := delayAlert("2026-10-16T08:00:00Z", "EV-1")
		s, b := summarizerWith(t, m, alert)
		s.Language = lang
		s.Summarize(context.Background(), b, alert)
		sys := m.Calls()[0].System
		if got := strings.Contains(sys, "Brazilian Portuguese"); got != want {
			t.Errorf("%q: the rule %v, want %v", lang, got, want)
		}
		// Among the rules: the reply's format still comes last.
		if !strings.HasSuffix(sys, `{"notification": "...", "citedEventIds": ["..."]}.`) {
			t.Errorf("%q: the format isn't last:\n%s", lang, sys)
		}
	}
}
