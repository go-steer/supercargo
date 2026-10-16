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

package webui

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// TestBrand: the brand line comes with who's signed in, and only when set.
func TestBrand(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	for brand, want := range map[string]string{"on a platform": "on a platform", "": ""} {
		h := Handler(Config{Company: "Serra Transportes", Domain: "serra-transportes.example", Brand: brand, Personas: personas, Control: &echo{},
			Now: func() time.Time { return now }})
		var me MeResponse
		_ = json.Unmarshal(do(h, http.MethodGet, "/ui/api/me", "", login(t, h, "sam")).Body.Bytes(), &me)
		if me.Brand != want {
			t.Errorf("brand %q, want %q", me.Brand, want)
		}
	}
}
