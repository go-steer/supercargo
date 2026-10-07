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

package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/go-steer/supercargo/internal/llm"
)

const toolsReply = `{"id":"msg_3","type":"message","role":"assistant","model":"claude-test","stop_reason":"tool_use",
"content":[{"type":"text","text":"Let me check."},
 {"type":"tool_use","id":"toolu_1","name":"get_events","input":{"bookingRef":"OB-BKG-1042"}},
 {"type":"tool_use","id":"toolu_2","name":"list_shipments","input":{}}],
"usage":{"input_tokens":50,"output_tokens":20}}`

// TestToolRoundTrip: tool definitions, earlier calls and their results go
// out; the model's new calls come back, in order, with their IDs.
func TestToolRoundTrip(t *testing.T) {
	api := &fakeAPI{}
	srv := api.server(t, http.StatusOK, toolsReply)
	m, _ := New(Options{APIKey: "k", BaseURL: srv.URL})
	out, err := m.Generate(context.Background(), llm.GenerateRequest{
		Tools: []llm.Tool{{Name: "get_events", Description: "A booking's events.",
			Schema: []byte(`{"type":"object","properties":{"bookingRef":{"type":"string"}},"required":["bookingRef"],"additionalProperties":false}`)}},
		Messages: []llm.Message{
			{Role: llm.User, Text: "Where is it?"},
			{Role: llm.Assistant, Text: "Checking.", ToolCalls: []llm.ToolCall{{ID: "toolu_0", Name: "list_shipments"}, {Name: "get_events", Input: []byte(`{"bookingRef":"X"}`)}}},
			{Role: llm.User, ToolResults: []llm.ToolResult{{CallID: "toolu_0", Content: `[]`}, {CallID: "call_get_events_2", Content: "refused", IsError: true}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.StopReason != llm.StopTool || out.Text != "Let me check." || len(out.ToolCalls) != 2 ||
		out.ToolCalls[0].ID != "toolu_1" || out.ToolCalls[0].Name != "get_events" || string(out.ToolCalls[0].Input) != `{"bookingRef":"OB-BKG-1042"}` ||
		out.ToolCalls[1].Name != "list_shipments" {
		t.Errorf("response %+v", out)
	}

	body := api.bodies[0]
	tool := body["tools"].([]any)[0].(map[string]any)
	if tool["name"] != "get_events" || tool["input_schema"].(map[string]any)["additionalProperties"] != false || body["tool_choice"] != nil {
		t.Errorf("tools %v, tool_choice %v", body["tools"], body["tool_choice"])
	}
	msgs := body["messages"].([]any)
	asst := msgs[1].(map[string]any)["content"].([]any)
	if len(asst) != 3 || asst[1].(map[string]any)["type"] != "tool_use" || asst[1].(map[string]any)["id"] != "toolu_0" ||
		asst[2].(map[string]any)["id"] != "call_get_events_2" {
		t.Errorf("assistant blocks %v", asst)
	}
	if in, _ := json.Marshal(asst[1].(map[string]any)["input"]); string(in) != `{}` {
		t.Errorf("a call with no input sent %s", in)
	}
	results := msgs[2].(map[string]any)["content"].([]any)
	if len(results) != 2 || results[1].(map[string]any)["type"] != "tool_result" || results[1].(map[string]any)["is_error"] != true ||
		results[1].(map[string]any)["tool_use_id"] != "call_get_events_2" {
		t.Errorf("result blocks %v", results)
	}
}

func TestToolRequestRejects(t *testing.T) {
	m, _ := New(Options{APIKey: "k", BaseURL: "http://127.0.0.1:1"})
	tool := []llm.Tool{{Name: "t", Schema: []byte(`{"type":"object"}`)}}
	for name, req := range map[string]llm.GenerateRequest{
		"schema and tools":           {JSONSchema: []byte(`{"type":"object"}`), Tools: tool, Messages: []llm.Message{{Role: llm.User, Text: "x"}}},
		"a tool with a bad schema":   {Tools: []llm.Tool{{Name: "t", Schema: []byte(`[`)}}, Messages: []llm.Message{{Role: llm.User, Text: "x"}}},
		"calls from the user":        {Messages: []llm.Message{{Role: llm.User, ToolCalls: []llm.ToolCall{{ID: "1", Name: "t"}}}}},
		"results from the assistant": {Messages: []llm.Message{{Role: llm.Assistant, ToolResults: []llm.ToolResult{{CallID: "1"}}}}},
		"invalid call input":         {Messages: []llm.Message{{Role: llm.Assistant, ToolCalls: []llm.ToolCall{{ID: "1", Name: "t", Input: []byte(`{`)}}}}},
	} {
		if _, err := m.params(req); err == nil {
			t.Errorf("%s: params succeeded", name)
		}
	}
	// The API refuses empty text blocks, so an empty message is an error.
	if _, err := m.params(llm.GenerateRequest{Messages: []llm.Message{{Role: llm.User}}}); err == nil {
		t.Error("an empty message was sent")
	}
	// A call's input must be an object.
	if _, err := m.params(llm.GenerateRequest{Messages: []llm.Message{{Role: llm.Assistant,
		ToolCalls: []llm.ToolCall{{ID: "1", Name: "t", Input: []byte(`[]`)}}}}}); err == nil {
		t.Error("a call with array input was sent")
	}
}

// TestToolResultsComeFirst: in a user message, tool results precede any
// text, as the API requires.
func TestToolResultsComeFirst(t *testing.T) {
	m, _ := New(Options{APIKey: "k", BaseURL: "http://127.0.0.1:1"})
	p, err := m.params(llm.GenerateRequest{Messages: []llm.Message{
		{Role: llm.Assistant, ToolCalls: []llm.ToolCall{{ID: "toolu_1", Name: "t", Input: []byte(`{}`)}}},
		{Role: llm.User, Text: "And also:", ToolResults: []llm.ToolResult{{CallID: "toolu_1", Content: "ok"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	blocks := p.Messages[1].Content
	if len(blocks) != 2 || blocks[0].OfToolResult == nil || blocks[1].OfText == nil {
		t.Errorf("user blocks: result first %v, then text %v", blocks[0].OfToolResult != nil, len(blocks) > 1 && blocks[1].OfText != nil)
	}
}
