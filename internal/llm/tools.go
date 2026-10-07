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

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// Tool is a tool the model may call: a name, what it does, and a JSON
// Schema for its input (an object). Models propose; the caller validates
// each call against the schema and decides whether to run it (DESIGN
// A.10: the tool loop is ours).
type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage
}

// ToolCall is a call the model proposed.
type ToolCall struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// ToolResult is what the caller tells the model a call returned.
type ToolResult struct {
	CallID  string
	Content string
	IsError bool
}

// ErrUnknownTool and ErrBadInput report calls that must not run; their
// text is safe to show the model. ErrBadSchema reports a tool whose schema
// this package can't enforce: a configuration error, never sent back to the
// model as its fault.
var (
	ErrUnknownTool = errors.New("llm: no such tool")
	ErrBadInput    = errors.New("llm: tool input doesn't match its schema")
	ErrBadSchema   = errors.New("llm: tool schema can't be enforced")
)

// MaxToolInput bounds a call's input.
const MaxToolInput = 64 << 10

// ValidateCall checks call against tools: the tool exists, and the input is
// one JSON object that matches the tool's schema. The schema subset checked
// is the one our tools use: type object with properties (string, integer,
// number, boolean, array of strings, object), required, additionalProperties
// false, string maxLength and enum, integer minimum and maximum, array
// maxItems. A call that fails must not run; the error is safe to show the
// model.
func ValidateCall(tools []Tool, call ToolCall) error {
	i := slices.IndexFunc(tools, func(t Tool) bool { return t.Name == call.Name })
	if i < 0 {
		return fmt.Errorf("%w: %s", ErrUnknownTool, strconv.Quote(call.Name))
	}
	if len(call.Input) > MaxToolInput {
		return fmt.Errorf("%w: input over %d bytes", ErrBadInput, MaxToolInput)
	}
	schema, err := parseSchema(tools[i].Schema)
	if err != nil {
		// The tool's fault, not the model's: not ErrBadInput.
		return fmt.Errorf("%w: tool %s: %v", ErrBadSchema, call.Name, err)
	}
	input, err := decodeObject(call.Input)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBadInput, err)
	}
	if err := schema.check("input", input); err != nil {
		return fmt.Errorf("%w: %v", ErrBadInput, err)
	}
	return nil
}

// schemaNode is the JSON Schema subset ValidateCall understands.
type schemaNode struct {
	Type                 string                `json:"type"`
	Properties           map[string]schemaNode `json:"properties"`
	Required             []string              `json:"required"`
	AdditionalProperties *bool                 `json:"additionalProperties"`
	Items                *schemaNode           `json:"items"`
	MaxLength            *int                  `json:"maxLength"`
	MaxItems             *int                  `json:"maxItems"`
	Minimum              *float64              `json:"minimum"`
	Maximum              *float64              `json:"maximum"`
	Enum                 []any                 `json:"enum"`
	// Annotations, accepted and ignored.
	Description string `json:"description"`
	Title       string `json:"title"`
	SchemaURI   string `json:"$schema"`
}

// parseSchema decodes a tool's schema strictly: the root is an object
// schema, and every keyword is one ValidateCall enforces (or an annotation).
// A keyword it would ignore, such as pattern or minLength, is an error:
// failing closed beats a schema author believing a constraint is checked.
func parseSchema(raw json.RawMessage) (schemaNode, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	var s schemaNode
	if err := dec.Decode(&s); err != nil {
		return s, err
	}
	if s.Type != "object" {
		return s, errors.New(`the root must be {"type": "object"}`)
	}
	return s, s.supported("schema")
}

func (s schemaNode) supported(path string) error {
	switch s.Type {
	case "object", "string", "integer", "number", "boolean", "array", "":
	default:
		return fmt.Errorf("%s: type %s isn't supported", path, strconv.Quote(s.Type))
	}
	for k, p := range s.Properties {
		if err := p.supported(path + "." + k); err != nil {
			return err
		}
	}
	if s.Items != nil {
		return s.Items.supported(path + "[]")
	}
	return nil
}

func (s schemaNode) check(path string, v any) error {
	switch s.Type {
	case "object":
		m, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("%s is not an object", path)
		}
		for _, r := range s.Required {
			if _, ok := m[r]; !ok {
				return fmt.Errorf("%s.%s is required", path, r)
			}
		}
		for k, val := range m {
			prop, ok := s.Properties[k]
			if !ok {
				// encoding/json matches member names case-insensitively, so
				// a tool decoding this would read "Count" as "count": refuse
				// any near-miss of a declared name, whatever
				// additionalProperties says.
				for name := range s.Properties {
					if strings.EqualFold(k, name) {
						return fmt.Errorf("%s has %s, which differs from %s only in case", path, strconv.Quote(k), strconv.Quote(name))
					}
				}
				if s.AdditionalProperties != nil && !*s.AdditionalProperties {
					return fmt.Errorf("%s has an unexpected member %s", path, strconv.Quote(k))
				}
				continue
			}
			if err := prop.check(path+"."+k, val); err != nil {
				return err
			}
		}
	case "string":
		str, ok := v.(string)
		if !ok {
			return fmt.Errorf("%s is not a string", path)
		}
		if s.MaxLength != nil && len([]rune(str)) > *s.MaxLength {
			return fmt.Errorf("%s is over %d characters", path, *s.MaxLength)
		}
	case "integer", "number":
		n, ok := v.(json.Number)
		if !ok {
			return fmt.Errorf("%s is not a number", path)
		}
		if s.Type == "integer" {
			if _, err := n.Int64(); err != nil {
				return fmt.Errorf("%s is not an integer", path)
			}
		}
		// A number too large for a float64 (1e400) is out of any range.
		f, err := n.Float64()
		if err != nil || s.Minimum != nil && f < *s.Minimum || s.Maximum != nil && f > *s.Maximum {
			return fmt.Errorf("%s is out of range", path)
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("%s is not a boolean", path)
		}
	case "array":
		a, ok := v.([]any)
		if !ok {
			return fmt.Errorf("%s is not an array", path)
		}
		if s.MaxItems != nil && len(a) > *s.MaxItems {
			return fmt.Errorf("%s has over %d items", path, *s.MaxItems)
		}
		if s.Items != nil {
			for i, item := range a {
				if err := s.Items.check(fmt.Sprintf("%s[%d]", path, i), item); err != nil {
					return err
				}
			}
		}
	}
	if len(s.Enum) > 0 && !slices.ContainsFunc(s.Enum, func(e any) bool { return sameJSON(e, v) }) {
		return fmt.Errorf("%s is not one of the allowed values", path)
	}
	return nil
}

// decodeObject decodes one JSON object, numbers kept exact, refusing
// duplicate members (which one would the tool see?) and trailing data.
func decodeObject(raw json.RawMessage) (map[string]any, error) {
	if err := noDuplicates(raw); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil || m == nil {
		return nil, errors.New("not a JSON object")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("more after the object")
	}
	return m, nil
}

// noDuplicates walks raw's tokens and refuses an object with a repeated
// member name, at any depth.
func noDuplicates(raw json.RawMessage) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	type frame struct {
		obj  bool
		seen map[string]bool
		key  bool // the next string token is a key
	}
	var stack []*frame
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return errors.New("not valid JSON")
		}
		top := func() *frame {
			if len(stack) == 0 {
				return nil
			}
			return stack[len(stack)-1]
		}
		if f := top(); f != nil && f.obj && f.key {
			if k, ok := tok.(string); ok {
				// Folded, as encoding/json matches names: "a" and "A" would be
				// one member to a tool.
				folded := foldKey(k)
				if f.seen[folded] {
					return fmt.Errorf("member %s appears twice (names compared without case)", strconv.Quote(k))
				}
				f.seen[folded] = true
				f.key = false
				continue
			}
		}
		switch tok {
		case json.Delim('{'):
			stack = append(stack, &frame{obj: true, seen: map[string]bool{}, key: true})
			continue
		case json.Delim('['):
			stack = append(stack, &frame{})
			continue
		case json.Delim('}'), json.Delim(']'):
			stack = stack[:len(stack)-1]
		}
		// A value finished: in an object, a key comes next.
		if f := top(); f != nil && f.obj {
			f.key = true
		}
	}
}

// sameJSON reports whether a and b encode to the same JSON: 1 and "1" differ.
func sameJSON(a, b any) bool {
	ja, err1 := json.Marshal(a)
	jb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && bytes.Equal(ja, jb)
}

// foldKey maps each rune of s to the smallest rune in its Unicode simple
// case-folding orbit, so names encoding/json would match (including the
// Kelvin sign for k and the long s for s) map to the same key.
func foldKey(s string) string {
	var b strings.Builder
	for _, r := range s {
		least := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			least = min(least, f)
		}
		b.WriteRune(least)
	}
	return b.String()
}
