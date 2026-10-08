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

package gemini

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

// fakeAPI is the Gemini API's generateContent: it records each request
// body and answers with the next canned reply.
type fakeAPI struct {
	mu      sync.Mutex
	paths   []string
	bodies  []map[string]any
	replies []string
	status  int
}

func (f *fakeAPI) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		f.paths, f.bodies = append(f.paths, r.URL.Path), append(f.bodies, body)
		w.Header().Set("Content-Type", "application/json")
		if f.status != 0 {
			w.WriteHeader(f.status)
		}
		reply := `{}`
		if len(f.replies) > 0 {
			reply, f.replies = f.replies[0], f.replies[1:]
		}
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newModel(t *testing.T, f *fakeAPI) *Model {
	t.Helper()
	m, err := New(context.Background(), Options{Model: "gemini-test", APIKey: "k", BaseURL: f.server(t).URL})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

var etaTool = llm.Tool{Name: "get_eta", Description: "The ETA.",
	Schema: json.RawMessage(`{"type":"object","properties":{"bookingRef":{"type":"string"}},"required":["bookingRef"]}`)}

// TestToolRoundTrip: a function call comes back as a tool call with its
// thought signature, and goes back out (with its result, by name) with the
// same signature, as Gemini requires.
func TestToolRoundTrip(t *testing.T) {
	f := &fakeAPI{replies: []string{
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"Let me look."},{"functionCall":{"name":"get_eta","args":{"bookingRef":"OB-1"}},"thoughtSignature":"c2lnbmVk"}]},"finishReason":"STOP"}],
		  "usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":3},"modelVersion":"gemini-test-001"}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"Thinking…","thought":true},{"text":"Friday."}]},"finishReason":"STOP"}]}`,
	}}
	m := newModel(t, f)
	if m.Name() != "gemini:gemini-test" {
		t.Errorf("Name = %s", m.Name())
	}
	msgs := []llm.Message{{Role: llm.User, Text: "When?"}}
	r, err := m.Generate(context.Background(), llm.GenerateRequest{System: "Be brief.", Messages: msgs, Tools: []llm.Tool{etaTool}})
	if err != nil || r.StopReason != llm.StopTool || len(r.ToolCalls) != 1 || r.ToolCalls[0].Name != "get_eta" ||
		string(r.ToolCalls[0].Input) != `{"bookingRef":"OB-1"}` || string(r.ToolCalls[0].Signature) != "signed" ||
		r.Text != "Let me look." || r.Model != "gemini-test-001" || r.Usage.InputTokens != 7 || r.Usage.OutputTokens != 3 {
		t.Fatalf("first turn: %+v, %v", r, err)
	}
	if !strings.HasSuffix(f.paths[0], "/models/gemini-test:generateContent") {
		t.Errorf("path %s", f.paths[0])
	}
	body := f.bodies[0]
	sys := body["systemInstruction"].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"]
	decl := body["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any)[0].(map[string]any)
	if sys != "Be brief." || decl["name"] != "get_eta" || decl["parametersJsonSchema"] == nil {
		t.Errorf("request: system %v, tool %v", sys, decl)
	}

	msgs = append(msgs, llm.Message{Role: llm.Assistant, Text: r.Text, ToolCalls: r.ToolCalls},
		llm.Message{Role: llm.User, ToolResults: []llm.ToolResult{{CallID: r.ToolCalls[0].ID, Content: "Friday"}}})
	r, err = m.Generate(context.Background(), llm.GenerateRequest{Messages: msgs, Tools: []llm.Tool{etaTool}})
	if err != nil || r.Text != "Friday." || r.StopReason != llm.StopEnd || r.Model != "gemini-test" {
		t.Fatalf("second turn (thoughts are not the reply): %+v, %v", r, err)
	}
	contents := f.bodies[1]["contents"].([]any)
	model := contents[1].(map[string]any)
	call := model["parts"].([]any)[1].(map[string]any)
	if model["role"] != "model" || call["thoughtSignature"] != "c2lnbmVk" || call["functionCall"].(map[string]any)["name"] != "get_eta" {
		t.Errorf("the call went back without its signature: %v", model)
	}
	resp := contents[2].(map[string]any)["parts"].([]any)[0].(map[string]any)["functionResponse"].(map[string]any)
	if resp["name"] != "get_eta" || resp["response"].(map[string]any)["output"] != "Friday" {
		t.Errorf("the result: %v", resp)
	}
}

func TestJSONAndStops(t *testing.T) {
	f := &fakeAPI{replies: []string{
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"{\"port\":\"NLRTM\"}"}]},"finishReason":"STOP"}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"{\"po"}]},"finishReason":"MAX_TOKENS"}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"cut"}]},"finishReason":"MAX_TOKENS"}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"x"}]},"finishReason":"SAFETY"}]}`,
		`{"candidates":[]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"get_eta"}}]},"finishReason":"STOP"}]}`,
	}}
	m := newModel(t, f)
	schema := json.RawMessage(`{"type":"object","properties":{"port":{"type":"string"}}}`)
	q := []llm.Message{{Role: llm.User, Text: "Rotterdam?"}}
	r, err := m.Generate(context.Background(), llm.GenerateRequest{JSONSchema: schema, Messages: q, MaxTokens: 50})
	cfg := f.bodies[0]["generationConfig"].(map[string]any)
	if err != nil || r.Text != `{"port":"NLRTM"}` || cfg["responseMimeType"] != "application/json" || cfg["responseJsonSchema"] == nil || cfg["maxOutputTokens"] != float64(50) {
		t.Errorf("JSON: %+v, %v, config %v", r, err, cfg)
	}
	if _, err := m.Generate(context.Background(), llm.GenerateRequest{JSONSchema: schema, Messages: q}); err == nil {
		t.Error("a JSON reply cut at max tokens was taken")
	}
	if r, err := m.Generate(context.Background(), llm.GenerateRequest{Messages: q}); err != nil || r.StopReason != llm.StopMaxTokens {
		t.Errorf("a text reply cut at max tokens: %+v, %v", r, err)
	}
	if _, err := m.Generate(context.Background(), llm.GenerateRequest{Messages: q}); err == nil || !strings.Contains(err.Error(), "SAFETY") {
		t.Errorf("a blocked reply: %v", err)
	}
	if _, err := m.Generate(context.Background(), llm.GenerateRequest{Messages: q}); err == nil {
		t.Error("no candidates was taken")
	}
	if r, err := m.Generate(context.Background(), llm.GenerateRequest{Messages: q, Tools: []llm.Tool{etaTool}}); err != nil ||
		len(r.ToolCalls) != 1 || string(r.ToolCalls[0].Input) != "{}" || r.ToolCalls[0].ID == "" {
		t.Errorf("a call with no args or ID: %+v, %v", r, err)
	}
}

func TestRequestErrors(t *testing.T) {
	f := &fakeAPI{status: http.StatusUnauthorized, replies: []string{`{"error":{"code":401,"message":"no","status":"UNAUTHENTICATED"}}`}}
	m := newModel(t, f)
	ok := []llm.Message{{Role: llm.User, Text: "?"}}
	if _, err := m.Generate(context.Background(), llm.GenerateRequest{Messages: ok}); err == nil {
		t.Error("an API error was taken")
	}
	for name, req := range map[string]llm.GenerateRequest{
		"schema and tools":        {Messages: ok, JSONSchema: json.RawMessage(`{}`), Tools: []llm.Tool{etaTool}},
		"a bad tool schema":       {Messages: ok, Tools: []llm.Tool{{Name: "x", Schema: json.RawMessage(`{`)}}},
		"a bad JSON schema":       {Messages: ok, JSONSchema: json.RawMessage(`{`)},
		"an unknown role":         {Messages: []llm.Message{{Role: "system", Text: "x"}}},
		"an empty message":        {Messages: []llm.Message{{Role: llm.User}}},
		"a result with no call":   {Messages: []llm.Message{{Role: llm.User, ToolResults: []llm.ToolResult{{CallID: "c1", Content: "x"}}}}},
		"results from the model":  {Messages: []llm.Message{{Role: llm.Assistant, ToolResults: []llm.ToolResult{{CallID: "c1"}}}}},
		"calls from the user":     {Messages: []llm.Message{{Role: llm.User, ToolCalls: []llm.ToolCall{{Name: "x"}}}}},
		"a call that's no object": {Messages: []llm.Message{{Role: llm.Assistant, ToolCalls: []llm.ToolCall{{Name: "x", Input: json.RawMessage(`[1]`)}}}}},
	} {
		if _, err := m.Generate(context.Background(), req); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// An error result is sent as one; a call with no ID gets one, and its
	// result must use it.
	f2 := &fakeAPI{replies: []string{`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`}}
	m2 := newModel(t, f2)
	_, err := m2.Generate(context.Background(), llm.GenerateRequest{Messages: []llm.Message{
		{Role: llm.User, Text: "?"}, {Role: llm.Assistant, ToolCalls: []llm.ToolCall{{Name: "get_eta"}}},
		{Role: llm.User, ToolResults: []llm.ToolResult{{CallID: "call_get_eta_1", Content: "broken", IsError: true}}}}})
	part := f2.bodies[0]["contents"].([]any)[2].(map[string]any)["parts"].([]any)[0].(map[string]any)
	if err != nil || part["functionResponse"].(map[string]any)["response"].(map[string]any)["error"] != "broken" {
		t.Errorf("an error result: %v, %v", part, err)
	}
}

// hostRecorder answers every request itself (no network) and records the
// URL and headers it was sent with.
type hostRecorder struct {
	mu   sync.Mutex
	reqs []*http.Request
}

func (h *hostRecorder) RoundTrip(r *http.Request) (*http.Response, error) {
	h.mu.Lock()
	h.reqs = append(h.reqs, r)
	h.mu.Unlock()
	body := `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

// TestEndpointNotFromEnvironment: the SDK's base URL variables (and its
// backend and key variables) can't send the key or the token elsewhere:
// the endpoint is always Google's.
func TestEndpointNotFromEnvironment(t *testing.T) {
	t.Setenv("GOOGLE_GEMINI_BASE_URL", "https://canary.example/")
	t.Setenv("GOOGLE_VERTEX_BASE_URL", "https://canary.example/")
	t.Setenv("GOOGLE_GENAI_USE_VERTEXAI", "true")
	t.Setenv("GOOGLE_API_KEY", "env-key")
	t.Setenv("GEMINI_API_KEY", "env-key")
	for name, c := range map[string]struct {
		opts Options
		host string
	}{
		"api key":           {Options{APIKey: "k"}, "generativelanguage.googleapis.com"},
		"vertex, region":    {Options{Vertex: &Vertex{Project: "p", Location: "us-east5"}}, "us-east5-aiplatform.googleapis.com"},
		"vertex, global":    {Options{Vertex: &Vertex{Project: "p", Location: "global"}}, "aiplatform.googleapis.com"},
		"vertex, multi-reg": {Options{Vertex: &Vertex{Project: "p", Location: "eu"}}, "aiplatform.eu.rep.googleapis.com"},
	} {
		rec := &hostRecorder{}
		c.opts.Model, c.opts.HTTPClient = "g", &http.Client{Transport: rec}
		m, err := New(context.Background(), c.opts)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := m.Generate(context.Background(), llm.GenerateRequest{Messages: []llm.Message{{Role: llm.User, Text: "?"}}}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		r := rec.reqs[0]
		if r.URL.Host != c.host {
			t.Errorf("%s: sent to %s", name, r.URL.Host)
		}
		if key := r.Header.Get("X-Goog-Api-Key"); (c.opts.Vertex != nil && key != "") || (c.opts.Vertex == nil && key != "k") {
			t.Errorf("%s: key header %q", name, key)
		}
		if c.opts.Vertex != nil && !strings.Contains(r.URL.Path, "/projects/p/locations/"+c.opts.Vertex.Location+"/publishers/google/models/g:generateContent") {
			t.Errorf("%s: path %s", name, r.URL.Path)
		}
	}
}

// TestJSONDropsToolCalls: a JSON request offers no tools, so a function
// call in the reply is never handed to the caller to run.
func TestJSONDropsToolCalls(t *testing.T) {
	f := &fakeAPI{replies: []string{
		`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"send_booking","args":{}}}]},"finishReason":"STOP"}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"send_booking","args":{}}},{"text":"{\"a\":1}"}]},"finishReason":"STOP"}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"x"}]},"finishReason":"MALFORMED_FUNCTION_CALL"}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"x"}]},"finishReason":"RECITATION"}]}`,
	}}
	m := newModel(t, f)
	req := llm.GenerateRequest{JSONSchema: json.RawMessage(`{"type":"object"}`), Messages: []llm.Message{{Role: llm.User, Text: "?"}}}
	if r, err := m.Generate(context.Background(), req); err == nil || len(r.ToolCalls) != 0 {
		t.Errorf("a call only: %+v, %v", r, err)
	}
	if r, err := m.Generate(context.Background(), req); err != nil || len(r.ToolCalls) != 0 || r.Text != `{"a":1}` || r.StopReason != llm.StopEnd {
		t.Errorf("a call and text: %+v, %v", r, err)
	}
	for _, reason := range []string{"MALFORMED_FUNCTION_CALL", "RECITATION"} {
		if _, err := m.Generate(context.Background(), req); err == nil || !strings.Contains(err.Error(), reason) {
			t.Errorf("%s: %v", reason, err)
		}
	}
}

func TestNew(t *testing.T) {
	for name, opts := range map[string]Options{
		"no model":                 {APIKey: "k"},
		"neither key nor Vertex":   {Model: "m"},
		"both":                     {Model: "m", APIKey: "k", Vertex: &Vertex{Project: "p", Location: "l"}},
		"Vertex with no project":   {Model: "m", Vertex: &Vertex{Location: "l"}},
		"Vertex with no location":  {Model: "m", Vertex: &Vertex{Project: "p"}},
		"a location that's a host": {Model: "m", Vertex: &Vertex{Project: "p", Location: "evil.example/#"}},
		"a project that's a path":  {Model: "m", Vertex: &Vertex{Project: "p/../x", Location: "us-east5"}},
	} {
		if _, err := New(context.Background(), opts); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
