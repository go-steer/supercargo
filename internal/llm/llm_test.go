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

func TestPromptHash(t *testing.T) {
	a := GenerateRequest{System: "s", Messages: []Message{{Role: User, Text: "hello"}}}
	b := GenerateRequest{System: "s", Messages: []Message{{Role: User, Text: "hello!"}}}
	// The separators keep "s" + "user" + "x" apart from "su" + "ser" + "x".
	c := GenerateRequest{System: "su", Messages: []Message{{Role: "ser", Text: "hello"}}}
	if h := PromptHash(a); len(h) != 16 || h != PromptHash(a) {
		t.Errorf("PromptHash = %q, not stable or not 16 hex digits", h)
	}
	if PromptHash(a) == PromptHash(b) || PromptHash(a) == PromptHash(c) {
		t.Error("different prompts share a hash")
	}
}

func TestStripCodeFence(t *testing.T) {
	for in, want := range map[string]string{
		`{"a":1}`:                    `{"a":1}`,
		"  {\"a\":1}\n":              `{"a":1}`,
		"```json\n{\"a\":1}\n```":    `{"a":1}`,
		"```\n{\"a\":1}\n```":        `{"a":1}`,
		"```{\"a\":1}```":            `{"a":1}`,
		"```{\"a\":\n1}```":          "{\"a\":\n1}",
		"Here you go: ```json {}```": "Here you go: ```json {}```",
		"```":                        "```",
		"``````":                     "",
	} {
		if got := StripCodeFence(in); got != want {
			t.Errorf("StripCodeFence(%q) = %q, want %q", in, got, want)
		}
	}
}
