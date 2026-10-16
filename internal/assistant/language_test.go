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

package assistant

import (
	"strings"
	"testing"
)

// TestLanguage: the shipper's and the carrier's assistants answer in the
// node's language; English needs no rule.
func TestLanguage(t *testing.T) {
	for _, a := range []*Assistant{{Node: &node{}, Language: "pt-BR"}, {Carrier: &carrierNode{}, Language: "pt-BR"}} {
		if !strings.Contains(a.system(), "Brazilian Portuguese") {
			t.Errorf("no rule: %s", a.system())
		}
		a.Language = ""
		if strings.Contains(a.system(), "Portuguese") {
			t.Error("a rule for English")
		}
	}
}
