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
// genai types onto internal/llm's Model, without streaming or tool calls,
// which this module's callers don't use yet.

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

	"github.com/go-steer/supercargo/internal/llm"
)

// DefaultModel answers when Options.Model is empty.
const DefaultModel = "claude-sonnet-5-5"

// DefaultMaxTokens caps output when the request doesn't.
const DefaultMaxTokens = 4096

// respondTool is the tool a JSON-output request forces: its input schema
// is the requested schema, so the reply is the tool call's input.
const respondTool = "respond"

// Options configures a Model.
type Options struct {
	APIKey string
	Model  string
	// CacheSystem marks the system prompt for prompt caching.
	CacheSystem bool
	// BaseURL and HTTPClient replace the API endpoint and transport
	// (tests).
	BaseURL    string
	HTTPClient *http.Client
}

// Model is a Claude model behind the llm.Model interface.
type Model struct {
	client      anthropic.Client
	model       string
	cacheSystem bool
}

// New returns a Model. The API key is required: a node configured for a
// real model must not start without one.
func New(opts Options) (*Model, error) {
	if opts.APIKey == "" {
		return nil, errors.New("anthropic: no API key")
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
	reqOpts := []option.RequestOption{option.WithoutEnvironmentDefaults(), option.WithHTTPClient(hc),
		option.WithAPIKey(opts.APIKey), option.WithMaxRetries(1)}
	if opts.BaseURL != "" {
		reqOpts = append(reqOpts, option.WithBaseURL(opts.BaseURL))
	}
	return &Model{client: anthropic.NewClient(reqOpts...), model: opts.Model, cacheSystem: opts.CacheSystem}, nil
}

// Name implements llm.Model.
func (m *Model) Name() string { return "anthropic:" + m.model }

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
	out := llm.GenerateResponse{Model: msg.Model,
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
			if tu := block.AsToolUse(); req.JSONSchema != nil && tu.Name == respondTool {
				out.Text = string(tu.Input)
				return out, nil
			}
		case "text":
			text.WriteString(block.AsText().Text)
		}
	}
	out.Text = text.String()
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
	for _, msg := range req.Messages {
		switch msg.Role {
		case llm.User:
			p.Messages = append(p.Messages, anthropic.NewUserMessage(anthropic.NewTextBlock(msg.Text)))
		case llm.Assistant:
			p.Messages = append(p.Messages, anthropic.NewAssistantMessage(anthropic.NewTextBlock(msg.Text)))
		default:
			return p, fmt.Errorf("anthropic: unknown role %q", msg.Role)
		}
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
		p.ToolChoice = anthropic.ToolChoiceParamOfTool(respondTool)
	}
	return p, nil
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
