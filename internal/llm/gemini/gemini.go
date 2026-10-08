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

// Package gemini is the llm.Model for Google's Gemini models, through the
// Gemini API (an API key) or Vertex AI (Application Default Credentials:
// a gcloud login, or a Cloud Run service's own identity, and no key).
//
// As with every model: it proposes, deterministic code decides. Its output
// is validated by the caller like any other model's.
package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"cloud.google.com/go/auth"
	"google.golang.org/genai"

	"github.com/go-steer/supercargo/internal/llm"
)

// DefaultMaxTokens caps a reply when the request sets no limit.
const DefaultMaxTokens = 4096

// Options configure a Model: exactly one of APIKey or Vertex.
type Options struct {
	Model  string
	APIKey string
	// Vertex, if set, reaches Gemini through Vertex AI.
	Vertex *Vertex
	// BaseURL and HTTPClient replace the endpoint and transport (tests).
	BaseURL    string
	HTTPClient *http.Client
}

// Vertex is where on Vertex AI to reach Gemini, and with what
// credentials (nil: Application Default Credentials).
type Vertex struct {
	Project, Location string
	Credentials       *auth.Credentials
}

// Model is a Gemini model.
type Model struct {
	client *genai.Client
	model  string
	name   string
}

// New makes a Model. Everything comes from opts, never the environment:
// the SDK would otherwise pick up GOOGLE_API_KEY and friends on its own.
func New(ctx context.Context, opts Options) (*Model, error) {
	if opts.Model == "" {
		return nil, errors.New("gemini: no model")
	}
	timeout := 90 * time.Second
	// The SDK's own client carries Vertex AI's OAuth: only a test's client
	// replaces it.
	cfg := &genai.ClientConfig{HTTPClient: opts.HTTPClient, HTTPOptions: genai.HTTPOptions{BaseURL: opts.BaseURL, Timeout: &timeout}}
	name := "gemini:" + opts.Model
	switch v := opts.Vertex; {
	case v != nil && opts.APIKey != "":
		return nil, errors.New("gemini: an API key or Vertex AI, not both")
	case v != nil:
		if v.Project == "" || v.Location == "" {
			return nil, errors.New("gemini: Vertex AI needs a project and a location")
		}
		cfg.Backend, cfg.Project, cfg.Location, cfg.Credentials = genai.BackendVertexAI, v.Project, v.Location, v.Credentials
		name = "vertex:gemini:" + opts.Model
	case opts.APIKey != "":
		cfg.Backend, cfg.APIKey = genai.BackendGeminiAPI, opts.APIKey
	default:
		return nil, errors.New("gemini: no API key and no Vertex AI project")
	}
	client, err := genai.NewClient(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("gemini: %w", err)
	}
	return &Model{client: client, model: opts.Model, name: name}, nil
}

// Name implements llm.Model.
func (m *Model) Name() string { return m.name }

// Generate implements llm.Model. A request with a JSONSchema asks for one
// JSON object (the caller still validates it); a request with tools may
// get tool calls back, with IDs this adapter assigns when Gemini gives
// none.
func (m *Model) Generate(ctx context.Context, req llm.GenerateRequest) (llm.GenerateResponse, error) {
	contents, cfg, err := m.request(req)
	if err != nil {
		return llm.GenerateResponse{}, err
	}
	res, err := m.client.Models.GenerateContent(ctx, m.model, contents, cfg)
	if err != nil {
		return llm.GenerateResponse{}, fmt.Errorf("gemini: %w", err)
	}
	out := llm.GenerateResponse{Model: res.ModelVersion, StopReason: llm.StopEnd}
	if out.Model == "" {
		out.Model = m.model
	}
	if u := res.UsageMetadata; u != nil {
		out.Usage = llm.Usage{InputTokens: int(u.PromptTokenCount), OutputTokens: int(u.CandidatesTokenCount)}
	}
	if len(res.Candidates) == 0 || res.Candidates[0].Content == nil {
		return out, errors.New("gemini: no reply (blocked, or empty)")
	}
	c := res.Candidates[0]
	switch c.FinishReason {
	case genai.FinishReasonStop, genai.FinishReasonUnspecified, "":
	case genai.FinishReasonMaxTokens:
		out.StopReason = llm.StopMaxTokens
		if req.JSONSchema != nil {
			return out, errors.New("gemini: the reply stopped early (max tokens)")
		}
	default:
		return out, fmt.Errorf("gemini: the reply stopped: %s", c.FinishReason)
	}
	var text strings.Builder
	for i, p := range c.Content.Parts {
		switch {
		case p.FunctionCall != nil:
			args, err := json.Marshal(p.FunctionCall.Args)
			if err != nil || len(p.FunctionCall.Args) == 0 {
				args = json.RawMessage("{}")
			}
			id := p.FunctionCall.ID
			if id == "" {
				id = fmt.Sprintf("call_%s_%d", p.FunctionCall.Name, i+1)
			}
			// The thought signature must come back with the call.
			out.ToolCalls = append(out.ToolCalls, llm.ToolCall{ID: id, Name: p.FunctionCall.Name, Input: args,
				Signature: append([]byte(nil), p.ThoughtSignature...)})
		case p.Text != "" && !p.Thought:
			text.WriteString(p.Text)
		}
	}
	out.Text = text.String()
	if len(out.ToolCalls) > 0 {
		out.StopReason = llm.StopTool
	}
	return out, nil
}

func (m *Model) request(req llm.GenerateRequest) ([]*genai.Content, *genai.GenerateContentConfig, error) {
	if req.JSONSchema != nil && len(req.Tools) > 0 {
		return nil, nil, errors.New("gemini: a request has either a JSON schema or tools, not both")
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}
	cfg := &genai.GenerateContentConfig{MaxOutputTokens: int32(maxTokens)}
	if req.System != "" {
		cfg.SystemInstruction = &genai.Content{Parts: []*genai.Part{{Text: req.System}}}
	}
	for _, t := range req.Tools {
		var schema any
		if err := json.Unmarshal(t.Schema, &schema); err != nil {
			return nil, nil, fmt.Errorf("gemini: tool %s: the schema is not JSON: %w", t.Name, err)
		}
		if len(cfg.Tools) == 0 {
			cfg.Tools = []*genai.Tool{{}}
		}
		cfg.Tools[0].FunctionDeclarations = append(cfg.Tools[0].FunctionDeclarations,
			&genai.FunctionDeclaration{Name: t.Name, Description: t.Description, ParametersJsonSchema: schema})
	}
	if req.JSONSchema != nil {
		var schema any
		if err := json.Unmarshal(req.JSONSchema, &schema); err != nil {
			return nil, nil, fmt.Errorf("gemini: the JSON schema is not JSON: %w", err)
		}
		cfg.ResponseMIMEType, cfg.ResponseJsonSchema = "application/json", schema
	}
	// Tool results name their call by ID; Gemini wants the function's
	// name too, from the call that asked for it.
	names := map[string]string{}
	var contents []*genai.Content
	for _, msg := range req.Messages {
		c := &genai.Content{}
		switch msg.Role {
		case llm.User:
			c.Role = genai.RoleUser
		case llm.Assistant:
			c.Role = genai.RoleModel
		default:
			return nil, nil, fmt.Errorf("gemini: unknown role %q", msg.Role)
		}
		for _, r := range msg.ToolResults {
			if msg.Role != llm.User {
				return nil, nil, errors.New("gemini: only a user message carries tool results")
			}
			name, ok := names[r.CallID]
			if !ok {
				return nil, nil, fmt.Errorf("gemini: a result for call %s, which no earlier message made", r.CallID)
			}
			key := "output"
			if r.IsError {
				key = "error"
			}
			c.Parts = append(c.Parts, &genai.Part{FunctionResponse: &genai.FunctionResponse{ID: r.CallID, Name: name,
				Response: map[string]any{key: r.Content}}})
		}
		if msg.Text != "" {
			c.Parts = append(c.Parts, &genai.Part{Text: msg.Text})
		}
		for i, call := range msg.ToolCalls {
			if msg.Role != llm.Assistant {
				return nil, nil, errors.New("gemini: only an assistant message carries tool calls")
			}
			args := map[string]any{}
			if len(call.Input) > 0 {
				if err := json.Unmarshal(call.Input, &args); err != nil || args == nil {
					return nil, nil, fmt.Errorf("gemini: tool call %s's input is not a JSON object", call.Name)
				}
			}
			id := call.ID
			if id == "" {
				id = fmt.Sprintf("call_%s_%d", call.Name, i+1)
			}
			names[id] = call.Name
			c.Parts = append(c.Parts, &genai.Part{FunctionCall: &genai.FunctionCall{ID: id, Name: call.Name, Args: args},
				ThoughtSignature: call.Signature})
		}
		if len(c.Parts) == 0 {
			return nil, nil, errors.New("gemini: an empty message")
		}
		contents = append(contents, c)
	}
	return contents, cfg, nil
}
