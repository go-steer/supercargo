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

package config

import "testing"

// TestLanguage: a language tag in any case is the supported one's; an
// unsupported one is refused.
func TestLanguage(t *testing.T) {
	base := "domain: cymbal-shoes.example\nname: Cymbal\nrole: shipper\nlisten: \":8443\"\nkeys: [{ kid: cy-1, privateKeyFile: /k.pem }]\ntls: { mode: insecure-dev }\nllm: { provider: fake, language: "
	for in, want := range map[string]string{"pt-br": "pt-BR", "PT-BR": "pt-BR", "es": "es", "en": "en"} {
		c, err := Parse([]byte(base + in + " }\n"))
		if err != nil || c.LLM.Language != want {
			t.Errorf("%s: %v, %v", in, c, err)
		}
	}
	if _, err := Parse([]byte(base + "fr }\n")); err == nil {
		t.Error("fr accepted")
	}
}
