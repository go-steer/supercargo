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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-steer/supercargo/internal/llm"
)

// TestEnvironmentCannotRedirectTheKey: the SDK's environment defaults (a
// base URL, other credentials, extra headers) are off, so nothing in the
// environment can send the key elsewhere or add to what is sent.
func TestEnvironmentCannotRedirectTheKey(t *testing.T) {
	var trapped atomic.Int32
	trap := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { trapped.Add(1) }))
	t.Cleanup(trap.Close)
	t.Setenv("ANTHROPIC_BASE_URL", trap.URL)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "env-token")
	t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "X-Leak: yes")
	t.Setenv("ANTHROPIC_PROFILE", "nonexistent-profile")

	var auth, leak string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, leak = r.Header.Get("Authorization"), r.Header.Get("X-Leak")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(toolReply))
	}))
	t.Cleanup(api.Close)
	m, err := New(Options{APIKey: "sk-test", BaseURL: api.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Generate(context.Background(), llm.GenerateRequest{Messages: []llm.Message{{Role: llm.User, Text: "x"}}}); err != nil {
		t.Fatal(err)
	}
	if trapped.Load() != 0 || auth != "" || leak != "" {
		t.Errorf("the environment reached the request: trap hit %d times, Authorization %q, X-Leak %q", trapped.Load(), auth, leak)
	}

	// Without a BaseURL, requests go to Anthropic's production API and no
	// other host, whatever ANTHROPIC_BASE_URL says.
	var host string
	rt := roundTripper(func(r *http.Request) (*http.Response, error) {
		host = r.URL.Host
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(toolReply)), Request: r}, nil
	})
	prod, _ := New(Options{APIKey: "sk-test", HTTPClient: &http.Client{Transport: rt}})
	if _, err := prod.Generate(context.Background(), llm.GenerateRequest{Messages: []llm.Message{{Role: llm.User, Text: "x"}}}); err != nil {
		t.Fatal(err)
	}
	if host != "api.anthropic.com" || trapped.Load() != 0 {
		t.Errorf("production request went to %q (trap hit %d times)", host, trapped.Load())
	}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestStoppedEarly: a JSON reply cut short by max_tokens is an error, even
// if what arrived parses, so the caller's fallback runs.
func TestStoppedEarly(t *testing.T) {
	api := &fakeAPI{}
	srv := api.server(t, http.StatusOK, strings.Replace(toolReply, `"stop_reason":"tool_use"`, `"stop_reason":"max_tokens"`, 1))
	m, _ := New(Options{APIKey: "k", BaseURL: srv.URL})
	_, err := m.Generate(context.Background(), llm.GenerateRequest{Messages: []llm.Message{{Role: llm.User, Text: "x"}},
		JSONSchema: []byte(`{"type":"object"}`)})
	if err == nil || !strings.Contains(err.Error(), "max_tokens") {
		t.Errorf("a reply cut at max_tokens: %v, want an error", err)
	}
}
