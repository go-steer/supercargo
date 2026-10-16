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

import "testing"

// TestPrivateDateForms: a private date is caught however our people write
// it, in English, Portuguese or Spanish, accented or not; other dates and
// numbers pass.
func TestPrivateDateForms(t *testing.T) {
	a := &Assistant{Drafts: &Drafts{}, Private: []string{"2026-11-12", "multa contratual"}}
	for _, q := range []string{
		"A carga chega antes de 12 de novembro?", "até 12 de nov?", "Llega antes del 12 de noviembre?", "antes del 12 de nov",
		"até 12/11?", "até 12/11/2026?", "by 11/12", "até 12.11", "o dia 12", "el día 12", "entrega 12-11",
		"Há MULTA CONTRATUAL?", "por la multa contratual",
	} {
		if a.privateIn(q) == "" {
			t.Errorf("%q passed", q)
		}
	}
	for _, q := range []string{"A carga chega antes de 13 de novembro?", "dia 1 ou 2?", "até 112/110?", "When does the ship arrive?", "contêiner 45G1, 12000 kg"} {
		if p := a.privateIn(q); p != "" {
			t.Errorf("%q refused: %s", q, p)
		}
	}
}
