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

import "testing"

func TestCheckVertex(t *testing.T) {
	for _, ok := range [][2]string{{"my-proj-123", "us-east5"}, {"example.com:proj", "global"}, {"p", "eu"}} {
		if err := CheckVertex(ok[0], ok[1]); err != nil {
			t.Errorf("%v: %v", ok, err)
		}
	}
	for _, bad := range [][2]string{
		{"", "us-east5"}, {"p", ""},
		{"p", "evil.example/#"}, {"p", "evil.example"}, {"p", "us-east5@evil"}, {"p", "US-EAST5"},
		{"p/../x", "us-east5"}, {"p?x", "us-east5"}, {"p#", "us-east5"}, {"-p", "us-east5"},
	} {
		if err := CheckVertex(bad[0], bad[1]); err == nil {
			t.Errorf("%v: accepted", bad)
		}
	}
}

func TestVertexEndpoint(t *testing.T) {
	for loc, want := range map[string]string{
		"global":   "https://aiplatform.googleapis.com/",
		"us":       "https://aiplatform.us.rep.googleapis.com/",
		"eu":       "https://aiplatform.eu.rep.googleapis.com/",
		"us-east5": "https://us-east5-aiplatform.googleapis.com/",
	} {
		if got := VertexEndpoint(loc); got != want {
			t.Errorf("%s: %s", loc, got)
		}
	}
}
