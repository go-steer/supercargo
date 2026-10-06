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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-steer/supercargo/internal/llm"
)

// fakeAPI is a Messages API endpoint answering with reply (a message JSON)
// and status, recording each request body.
type fakeAPI struct {
	mu     sync.Mutex
	bodies []map[string]any
	keys   []string
}

func (f *fakeAPI) server(t *testing.T, status int, reply string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		f.mu.Lock()
		f.bodies = append(f.bodies, m)
		f.keys = append(f.keys, r.Header.Get("X-Api-Key"))
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return srv
}

const toolReply = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","stop_reason":"tool_use",
"content":[{"type":"text","text":"Here it is."},{"type":"tool_use","id":"tu_1","name":"respond","input":{"answer":"Tuesday.","citedEventIds":["EV-1"]}}],
"usage":{"input_tokens":120,"output_tokens":30}}`

func TestGenerateJSON(t *testing.T) {
	api := &fakeAPI{}
	srv := api.server(t, http.StatusOK, toolReply)
	m, err := New(Options{APIKey: "sk-test", Model: "claude-test", BaseURL: srv.URL, CacheSystem: true})
	if err != nil {
		t.Fatal(err)
	}
	schema := json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}`)
	out, err := m.Generate(context.Background(), llm.GenerateRequest{
		System:     "Be brief.",
		Messages:   []llm.Message{{Role: llm.User, Text: "When?"}, {Role: llm.Assistant, Text: "Checking."}, {Role: llm.User, Text: "Well?"}},
		JSONSchema: schema, MaxTokens: 256,
	})
	if err != nil {
		t.Fatal(err)
	}
	var reply map[string]any
	if err := json.Unmarshal([]byte(out.Text), &reply); err != nil || reply["answer"] != "Tuesday." {
		t.Errorf("text %q: %v", out.Text, err)
	}
	if out.Usage.InputTokens != 120 || out.Usage.OutputTokens != 30 || out.Model != "claude-test" || m.Name() != "anthropic:claude-test" {
		t.Errorf("response %+v, name %s", out, m.Name())
	}

	// What went over the wire.
	body := api.bodies[0]
	if api.keys[0] != "sk-test" || body["model"] != "claude-test" || body["max_tokens"] != float64(256) {
		t.Errorf("request %v (key %q)", body, api.keys[0])
	}
	sys := body["system"].([]any)[0].(map[string]any)
	if sys["text"] != "Be brief." || sys["cache_control"] == nil {
		t.Errorf("system %v", sys)
	}
	msgs := body["messages"].([]any)
	if len(msgs) != 3 || msgs[1].(map[string]any)["role"] != "assistant" {
		t.Errorf("messages %v", msgs)
	}
	tool := body["tools"].([]any)[0].(map[string]any)
	input := tool["input_schema"].(map[string]any)
	if tool["name"] != respondTool || input["type"] != "object" || input["additionalProperties"] != false ||
		input["required"].([]any)[0] != "answer" || input["properties"].(map[string]any)["answer"] == nil {
		t.Errorf("tool %v", tool)
	}
	if choice := body["tool_choice"].(map[string]any); choice["type"] != "tool" || choice["name"] != respondTool {
		t.Errorf("tool_choice %v", choice)
	}
}

func TestGenerateText(t *testing.T) {
	api := &fakeAPI{}
	srv := api.server(t, http.StatusOK, `{"id":"msg_2","type":"message","role":"assistant","model":"claude-test","stop_reason":"end_turn",
"content":[{"type":"text","text":"Tues"},{"type":"text","text":"day."},{"type":"tool_use","id":"tu","name":"other","input":{}}],"usage":{"input_tokens":1,"output_tokens":2}}`)
	m, _ := New(Options{APIKey: "k", BaseURL: srv.URL})
	out, err := m.Generate(context.Background(), llm.GenerateRequest{Messages: []llm.Message{{Role: llm.User, Text: "When?"}}})
	if err != nil || out.Text != "Tuesday." {
		t.Errorf("Generate = %+v, %v", out, err)
	}
	body := api.bodies[0]
	if body["model"] != DefaultModel || body["max_tokens"] != float64(DefaultMaxTokens) || body["tools"] != nil || body["system"] != nil {
		t.Errorf("request %v", body)
	}
}

func TestGenerateErrors(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Error("New without an API key succeeded")
	}
	api := &fakeAPI{}
	srv := api.server(t, http.StatusBadRequest, `{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`)
	m, _ := New(Options{APIKey: "k", BaseURL: srv.URL})
	if _, err := m.Generate(context.Background(), llm.GenerateRequest{Messages: []llm.Message{{Role: llm.User, Text: "x"}}}); err == nil ||
		!strings.HasPrefix(err.Error(), "anthropic:") {
		t.Errorf("a 400: %v", err)
	}
	if _, err := m.Generate(context.Background(), llm.GenerateRequest{Messages: []llm.Message{{Role: "system", Text: "x"}}}); err == nil {
		t.Error("an unknown role was sent")
	}
	if _, err := m.Generate(context.Background(), llm.GenerateRequest{Messages: []llm.Message{{Role: llm.User, Text: "x"}},
		JSONSchema: json.RawMessage(`[1]`)}); err == nil {
		t.Error("a schema that isn't an object was sent")
	}
	n := len(api.bodies)
	if n != 1 {
		t.Errorf("%d requests reached the API, want 1 (the 400)", n)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.Generate(ctx, llm.GenerateRequest{Messages: []llm.Message{{Role: llm.User, Text: "x"}}}); err == nil {
		t.Error("a cancelled request succeeded")
	}
}

func TestJSONSchemaToInput(t *testing.T) {
	props, required, extra, err := jsonSchemaToInput(json.RawMessage(`{"type":"object","$defs":{"x":{}},"required":["a",1]}`))
	if err != nil || len(props) != 0 || len(required) != 1 || extra["$defs"] == nil || extra["type"] != nil {
		t.Errorf("props %v required %v extra %v: %v", props, required, extra, err)
	}
}
