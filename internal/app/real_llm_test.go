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

package app

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/config"
	"github.com/go-steer/supercargo/internal/llm"
)

// TestRealModels calls real models: opt-in, never part of dev/tools/ci
// (AGENTS.md). With SUPERCARGO_REAL_LLM=1 and the providers' usual
// environment (a Vertex project and location, and Application Default
// Credentials), Gemini and Claude on Vertex AI each answer plainly, call a
// tool, and reply with a JSON object. SUPERCARGO_REAL_GEMINI_API=1 adds
// Gemini by API key (GEMINI_API_KEY).
//
//	SUPERCARGO_REAL_LLM=1 go test -run TestRealModels -v ./internal/app/
//
// SUPERCARGO_REAL_GEMINI_MODEL and SUPERCARGO_REAL_CLAUDE_MODEL pick the
// models.
func TestRealModels(t *testing.T) {
	if os.Getenv("SUPERCARGO_REAL_LLM") != "1" {
		t.Skip("real models: set SUPERCARGO_REAL_LLM=1")
	}
	gm := os.Getenv("SUPERCARGO_REAL_GEMINI_MODEL")
	if gm == "" {
		gm = "gemini-2.5-flash"
	}
	cases := map[string]config.LLM{
		"gemini on vertex": {Provider: config.ProviderGemini, Backend: config.BackendVertex, Model: gm},
		"claude on vertex": {Provider: config.ProviderAnthropic, Backend: config.BackendVertex, Model: os.Getenv("SUPERCARGO_REAL_CLAUDE_MODEL")},
	}
	if os.Getenv("SUPERCARGO_REAL_GEMINI_API") == "1" {
		cases["gemini by api key"] = config.LLM{Provider: config.ProviderGemini, Model: gm}
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := config.Config{LLM: &c}
			if err := cfg.LLM.ValidateForTest(); err != nil {
				t.Fatal(err)
			}
			m, err := NewModel(cfg.LLM, nil)
			if err != nil {
				t.Skipf("not configured here: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			r, err := m.Generate(ctx, llm.GenerateRequest{System: "Be brief.", MaxTokens: 1024,
				Messages: []llm.Message{{Role: llm.User, Text: "Reply with exactly the word: ready"}}})
			if err != nil {
				t.Fatalf("%s: %v", m.Name(), err)
			}
			t.Logf("%s (%s): %q, %d→%d tokens", m.Name(), r.Model, strings.TrimSpace(r.Text), r.Usage.InputTokens, r.Usage.OutputTokens)
			if !strings.Contains(strings.ToLower(r.Text), "ready") {
				t.Errorf("plain reply: %q", r.Text)
			}

			tool := llm.Tool{Name: "get_eta", Description: "The ETA of a booking.",
				Schema: json.RawMessage(`{"type":"object","properties":{"bookingRef":{"type":"string"}},"required":["bookingRef"],"additionalProperties":false}`)}
			msgs := []llm.Message{{Role: llm.User, Text: "When does booking OB-BKG-1042 arrive? Use the tool."}}
			r, err = m.Generate(ctx, llm.GenerateRequest{Messages: msgs, Tools: []llm.Tool{tool}, MaxTokens: 1024})
			if err != nil || len(r.ToolCalls) != 1 || r.ToolCalls[0].Name != "get_eta" || !strings.Contains(string(r.ToolCalls[0].Input), "OB-BKG-1042") {
				t.Fatalf("tool call: %+v, %v", r, err)
			}
			t.Logf("tool call: %s %s", r.ToolCalls[0].Name, r.ToolCalls[0].Input)
			msgs = append(msgs, llm.Message{Role: llm.Assistant, ToolCalls: r.ToolCalls},
				llm.Message{Role: llm.User, ToolResults: []llm.ToolResult{{CallID: r.ToolCalls[0].ID, Content: `{"eta":"2026-10-16T08:00:00Z"}`}}})
			r, err = m.Generate(ctx, llm.GenerateRequest{Messages: msgs, Tools: []llm.Tool{tool}, MaxTokens: 1024})
			if err != nil || !strings.Contains(r.Text, "16") {
				t.Fatalf("after the tool result: %q, %v", r.Text, err)
			}
			t.Logf("after the tool: %q", strings.TrimSpace(r.Text))

			r, err = m.Generate(ctx, llm.GenerateRequest{
				JSONSchema: json.RawMessage(`{"type":"object","properties":{"port":{"type":"string"}},"required":["port"]}`),
				// A short limit, as the summarizer's: a model that thinks
				// first must still have room to answer.
				MaxTokens: 200,
				Messages:  []llm.Message{{Role: llm.User, Text: "Which UN/LOCODE is Rotterdam? Answer as JSON."}}})
			var out struct{ Port string }
			if err != nil || json.Unmarshal([]byte(r.Text), &out) != nil || !strings.Contains(out.Port, "RTM") {
				t.Fatalf("JSON reply: %q, %v", r.Text, err)
			}
			t.Logf("JSON reply: %s", r.Text)
		})
	}
}
