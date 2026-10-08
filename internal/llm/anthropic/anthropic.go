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

// Derived from go-steer/mast@603b826132aaef26b1615809ebe5e7154d6b4b3f
// (pkg/providers/anthropic: convert.go's system blocks and JSON-schema
// projection, stream.go's usage mapping), ported from ADK's model.LLM and
// genai types onto internal/llm's Model, without streaming, which this
// module's callers don't use.

// Package anthropic is the llm.Model for Anthropic's Claude models
// (docs/DESIGN.md, A.10).
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/vertex"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/go-steer/supercargo/internal/llm"
)

// DefaultModel answers when Options.Model is empty.
const DefaultModel = "claude-sonnet-5-5"

// DefaultMaxTokens caps output when the request doesn't.
const DefaultMaxTokens = 4096

// respondTool is the tool a JSON-output request asks for: its input schema
// is the requested schema, so the reply is the tool call's input.
const respondTool = "respond"

// Options configures a Model.
type Options struct {
	APIKey string
	Model  string
	// CacheSystem marks the system prompt for prompt caching.
	CacheSystem bool
	// Vertex, if set, reaches Claude through Google Cloud's Vertex AI with
	// these credentials (Application Default Credentials, normally): no
	// API key.
	Vertex *Vertex
	// BaseURL and HTTPClient replace the API endpoint and transport
	// (tests).
	BaseURL    string
	HTTPClient *http.Client
}

// Vertex is where on Vertex AI to reach Claude, and with what credentials.
type Vertex struct {
	Project, Region string
	Credentials     *google.Credentials
}

// VertexScope is the OAuth scope Vertex AI needs.
const VertexScope = "https://www.googleapis.com/auth/cloud-platform"

// TokenTimeout bounds each OAuth token fetch on Vertex AI.
const TokenTimeout = 30 * time.Second

// DefaultCredentials finds Google's Application Default Credentials: a
// gcloud login, a service account key file, or (on Cloud Run) the
// service's own identity. They refresh tokens for as long as the model
// lives, so they are bound to no request's context; the refresh can't take
// a request's context either (oauth2's transport gives it none), so its
// HTTP client has a timeout of its own.
func DefaultCredentials() (*google.Credentials, error) {
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{Timeout: TokenTimeout})
	c, err := google.FindDefaultCredentials(ctx, VertexScope)
	if err != nil {
		return nil, fmt.Errorf("vertex: no Application Default Credentials (gcloud auth application-default login, or a service account): %w", err)
	}
	return c, nil
}

// withCredentials is the SDK's Vertex option (a test replaces it).
var withCredentials = vertex.WithCredentials

// vertexOption is the Vertex option, or an error where the SDK panics (it
// can't make its HTTP client): a node fails to start instead of crashing.
func vertexOption(v *Vertex) (o option.RequestOption, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("anthropic: vertex: %v", r)
		}
	}()
	return withCredentials(context.Background(), v.Region, v.Project, v.Credentials), nil
}

// Model is a Claude model behind the llm.Model interface.
type Model struct {
	client      anthropic.Client
	model       string
	cacheSystem bool
	name        string
}

// New returns a Model. The API key is required: a node configured for a
// real model must not start without one.
func New(opts Options) (*Model, error) {
	if opts.Vertex == nil && opts.APIKey == "" {
		return nil, errors.New("anthropic: no API key")
	}
	if v := opts.Vertex; v != nil {
		if v.Credentials == nil {
			return nil, errors.New("anthropic: Vertex AI needs credentials")
		}
		// The region becomes the endpoint's host.
		if err := llm.CheckVertex(v.Project, v.Region); err != nil {
			return nil, fmt.Errorf("anthropic: %w", err)
		}
	}
	if opts.Model == "" {
		opts.Model = DefaultModel
	}
	hc := opts.HTTPClient
	if hc == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.ResponseHeaderTimeout = 90 * time.Second
		hc = &http.Client{Transport: t}
	}
	// No environment defaults: the SDK would otherwise take a base URL
	// (ANTHROPIC_BASE_URL, a profile file), other credentials and extra
	// headers from the environment, and could send the key to another
	// host. The key goes to Anthropic's production API, or to BaseURL
	// when a test sets one, and nowhere else.
	reqOpts := []option.RequestOption{option.WithoutEnvironmentDefaults(), option.WithHTTPClient(hc), option.WithMaxRetries(1)}
	name := "anthropic:" + opts.Model
	if v := opts.Vertex; v != nil {
		// The HTTP client goes first: Vertex wraps it with OAuth. Its
		// option also turns off the SDK's environment defaults.
		vo, err := vertexOption(v)
		if err != nil {
			return nil, err
		}
		reqOpts = append(reqOpts, vo)
		name = "vertex:anthropic:" + opts.Model
	} else {
		reqOpts = append(reqOpts, option.WithAPIKey(opts.APIKey))
	}
	if opts.BaseURL != "" {
		reqOpts = append(reqOpts, option.WithBaseURL(opts.BaseURL))
	}
	return &Model{client: anthropic.NewClient(reqOpts...), model: opts.Model, cacheSystem: opts.CacheSystem, name: name}, nil
}

// Name implements llm.Model.
func (m *Model) Name() string { return m.name }

// Generate implements llm.Model. A request with a JSONSchema forces the
// respond tool, whose input is the reply; the caller still validates it.
func (m *Model) Generate(ctx context.Context, req llm.GenerateRequest) (llm.GenerateResponse, error) {
	params, err := m.params(req)
	if err != nil {
		return llm.GenerateResponse{}, err
	}
	msg, err := m.client.Messages.New(ctx, params)
	if err != nil {
		return llm.GenerateResponse{}, fmt.Errorf("anthropic: %w", err)
	}
	out := llm.GenerateResponse{Model: msg.Model, StopReason: string(msg.StopReason),
		Usage: llm.Usage{InputTokens: int(msg.Usage.InputTokens), OutputTokens: int(msg.Usage.OutputTokens)}}
	if req.JSONSchema != nil && msg.StopReason != "tool_use" && msg.StopReason != "end_turn" {
		// Cut short (max_tokens, say): the reply may still parse, with an
		// answer cut mid-thought or citations missing. It isn't one.
		return out, fmt.Errorf("anthropic: the reply stopped early (%s)", msg.StopReason)
	}
	var text strings.Builder
	for _, block := range msg.Content {
		switch block.Type {
		case "tool_use":
			tu := block.AsToolUse()
			if req.JSONSchema != nil {
				if tu.Name == respondTool {
					// The reply ends the turn: no tool calls for the caller
					// to run.
					out.Text, out.StopReason = string(tu.Input), llm.StopEnd
					return out, nil
				}
				continue
			}
			out.ToolCalls = append(out.ToolCalls, llm.ToolCall{ID: tu.ID, Name: tu.Name, Input: append(json.RawMessage(nil), tu.Input...)})
		case "text":
			text.WriteString(block.AsText().Text)
		}
	}
	out.Text = text.String()
	if req.JSONSchema != nil {
		// No respond call: a reply given as a JSON object in text is taken
		// (some models on Vertex AI can't be forced to call a tool); the
		// caller validates either way.
		t := llm.StripCodeFence(out.Text)
		var obj map[string]json.RawMessage
		if json.Unmarshal([]byte(t), &obj) != nil || obj == nil {
			return out, errors.New("anthropic: no reply: the model neither called respond nor answered with a JSON object")
		}
		out.Text, out.StopReason = t, llm.StopEnd
	}
	return out, nil
}

func (m *Model) params(req llm.GenerateRequest) (anthropic.MessageNewParams, error) {
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}
	p := anthropic.MessageNewParams{Model: m.model, MaxTokens: int64(maxTokens)}
	if req.System != "" {
		p.System = []anthropic.TextBlockParam{{Text: req.System}}
		if m.cacheSystem {
			p.System[0].CacheControl = anthropic.NewCacheControlEphemeralParam()
		}
	}
	if req.JSONSchema != nil && len(req.Tools) > 0 {
		return p, errors.New("anthropic: a request has either a JSON schema or tools, not both")
	}
	for _, msg := range req.Messages {
		blocks, err := messageBlocks(msg)
		if err != nil {
			return p, err
		}
		switch msg.Role {
		case llm.User:
			p.Messages = append(p.Messages, anthropic.NewUserMessage(blocks...))
		case llm.Assistant:
			p.Messages = append(p.Messages, anthropic.NewAssistantMessage(blocks...))
		default:
			return p, fmt.Errorf("anthropic: unknown role %q", msg.Role)
		}
	}
	for _, t := range req.Tools {
		props, required, extra, err := jsonSchemaToInput(t.Schema)
		if err != nil {
			return p, fmt.Errorf("anthropic: tool %s: %w", t.Name, err)
		}
		p.Tools = append(p.Tools, anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
			Name: t.Name, Description: anthropic.String(t.Description),
			InputSchema: anthropic.ToolInputSchemaParam{Properties: props, Required: required, ExtraFields: extra},
		}})
	}
	if req.JSONSchema != nil {
		props, required, extra, err := jsonSchemaToInput(req.JSONSchema)
		if err != nil {
			return p, err
		}
		p.Tools = []anthropic.ToolUnionParam{{OfTool: &anthropic.ToolParam{
			Name:        respondTool,
			Description: anthropic.String("Give your reply. Call this exactly once, with the whole reply as its input."),
			InputSchema: anthropic.ToolInputSchemaParam{Properties: props, Required: required, ExtraFields: extra},
		}}}
		// Asked, not forced: forcing a tool (tool_choice) isn't supported
		// by every model on Vertex AI. A text reply is still accepted.
		p.System = append(p.System, anthropic.TextBlockParam{Text: "Give your reply by calling the " + respondTool +
			" tool exactly once, with the whole reply as its input. Do not answer in text."})
	}
	return p, nil
}

// messageBlocks converts one message: its text, then (assistant) the tool
// calls it made, or (user) the results of the previous turn's calls. As in
// mast, a call without an ID gets one derived from its name and position,
// and its result must use the same.
func messageBlocks(msg llm.Message) ([]anthropic.ContentBlockParamUnion, error) {
	var out []anthropic.ContentBlockParamUnion
	// A user message's tool results come first: the API requires them
	// right after the assistant turn that made the calls, before any text.
	for _, r := range msg.ToolResults {
		if msg.Role != llm.User {
			return nil, errors.New("anthropic: only a user message carries tool results")
		}
		out = append(out, anthropic.NewToolResultBlock(r.CallID, r.Content, r.IsError))
	}
	if msg.Text != "" {
		out = append(out, anthropic.NewTextBlock(msg.Text))
	}
	for i, c := range msg.ToolCalls {
		if msg.Role != llm.Assistant {
			return nil, errors.New("anthropic: only an assistant message carries tool calls")
		}
		input := c.Input
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal(input, &obj) != nil || obj == nil {
			return nil, fmt.Errorf("anthropic: tool call %s's input is not a JSON object", c.Name)
		}
		id := c.ID
		if id == "" {
			id = fmt.Sprintf("call_%s_%d", c.Name, i+1)
		}
		out = append(out, anthropic.ContentBlockParamUnion{OfToolUse: &anthropic.ToolUseBlockParam{ID: id, Name: c.Name, Input: input}})
	}
	if len(out) == 0 {
		// The API refuses empty text blocks.
		return nil, errors.New("anthropic: an empty message")
	}
	return out, nil
}

// jsonSchemaToInput projects a JSON Schema object onto the (properties,
// required, extra fields) triple ToolInputSchemaParam expects; keys other
// than those three (additionalProperties, $defs) pass through verbatim.
func jsonSchemaToInput(schema json.RawMessage) (map[string]any, []string, map[string]any, error) {
	var generic map[string]any
	if err := json.Unmarshal(schema, &generic); err != nil {
		return nil, nil, nil, fmt.Errorf("anthropic: the JSON schema is not an object: %w", err)
	}
	props := map[string]any{}
	if p, ok := generic["properties"].(map[string]any); ok {
		props = p
	}
	var required []string
	if r, ok := generic["required"].([]any); ok {
		for _, v := range r {
			if s, ok := v.(string); ok {
				required = append(required, s)
			}
		}
	}
	var extra map[string]any
	for k, v := range generic {
		switch k {
		case "type", "properties", "required":
		default:
			if extra == nil {
				extra = map[string]any{}
			}
			extra[k] = v
		}
	}
	return props, required, extra, nil
}
