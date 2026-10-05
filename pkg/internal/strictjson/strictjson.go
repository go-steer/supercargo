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

// Package strictjson checks JSON that nobody has authenticated yet, and
// JSON whose meaning must be the same for every party that reads it.
//
// encoding/json is lenient in ways that matter there. It replaces invalid
// UTF-8 and lone surrogate escapes with U+FFFD, and lets the last of
// duplicate keys win where other parsers keep the first or fail. Valid
// refuses all of these, plus nesting deeper than MaxDepth, and it is cheap:
// one stdlib syntax pass and one byte scan, with no values built.
package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/go-steer/supercargo/pkg/internal/logsafe"
)

// MaxDepth bounds the nesting of arrays and objects.
const MaxDepth = 64

var errLoneSurrogate = errors.New("strictjson: string has an unpaired surrogate escape")

// Valid reports whether data is exactly one JSON value, in valid UTF-8,
// with no escaped lone surrogates, no duplicate object keys and nesting no
// deeper than MaxDepth.
func Valid(data []byte) error {
	if !utf8.Valid(data) {
		return errors.New("strictjson: input is not valid UTF-8")
	}
	// json.Valid checks the grammar, including that nothing but whitespace
	// follows the value, without recursion or allocation. Everything below
	// relies on it.
	if !json.Valid(data) {
		return errors.New("strictjson: not a single valid JSON value")
	}
	return scan(data)
}

// Decode checks data with Valid and decodes it into map[string]any, []any,
// string, bool, nil and json.Number.
func Decode(data []byte) (any, error) {
	if err := Valid(data); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("strictjson: %w", err)
	}
	return v, nil
}

// DecodeObject is Decode for input that must be a JSON object.
func DecodeObject(data []byte) (map[string]any, error) {
	v, err := Decode(data)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("strictjson: not a JSON object")
	}
	return m, nil
}

// scan walks syntactically valid JSON once, checking depth, duplicate keys
// and surrogate escapes. Validity is what keeps it simple: strings
// terminate, escapes are complete, and a string followed by ':' is a key of
// the innermost object.
func scan(data []byte) error {
	// One entry per open container: the keys seen so far for an object
	// (allocated on its first key), nil for an array.
	var stack []map[string]struct{}
	for i := 0; i < len(data); i++ {
		switch data[i] {
		case '{', '[':
			if len(stack) >= MaxDepth {
				return fmt.Errorf("strictjson: nesting deeper than %d", MaxDepth)
			}
			stack = append(stack, nil)
		case '}', ']':
			stack = stack[:len(stack)-1]
		case '"':
			start := i
			escaped := false
			for i++; data[i] != '"'; i++ {
				if data[i] != '\\' {
					continue
				}
				escaped = true
				i++
				if data[i] != 'u' {
					continue
				}
				n, err := checkUnicodeEscape(data, i)
				if err != nil {
					return err
				}
				i += n
			}
			j := i + 1
			for j < len(data) && isSpace(data[j]) {
				j++
			}
			if j == len(data) || data[j] != ':' {
				continue // a value, not a key
			}
			key := string(data[start+1 : i])
			if escaped {
				// The decoder's own unescaping, so keys compare exactly as
				// encoding/json will see them.
				if err := json.Unmarshal(data[start:i+1], &key); err != nil {
					return fmt.Errorf("strictjson: %w", err)
				}
			}
			top := len(stack) - 1
			if stack[top] == nil {
				stack[top] = map[string]struct{}{}
			}
			if _, dup := stack[top][key]; dup {
				return fmt.Errorf("strictjson: duplicate object key %s", logsafe.Quote(key))
			}
			stack[top][key] = struct{}{}
		}
	}
	return nil
}

// checkUnicodeEscape checks the \u escape whose 'u' is at data[i] and
// returns how many bytes past the 'u' it (and a low surrogate it pairs
// with) spans.
func checkUnicodeEscape(data []byte, i int) (int, error) {
	r := hex4(data[i+1 : i+5])
	switch {
	case r >= 0xd800 && r <= 0xdbff:
		// A high surrogate must be followed by an escaped low one.
		if i+10 >= len(data) || data[i+5] != '\\' || data[i+6] != 'u' {
			return 0, errLoneSurrogate
		}
		if lo := hex4(data[i+7 : i+11]); lo < 0xdc00 || lo > 0xdfff {
			return 0, errLoneSurrogate
		}
		return 10, nil
	case r >= 0xdc00 && r <= 0xdfff:
		return 0, errLoneSurrogate
	}
	return 4, nil
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// hex4 decodes four hex digits, which valid JSON guarantees.
func hex4(b []byte) rune {
	var r rune
	for _, c := range b {
		r <<= 4
		switch {
		case c >= '0' && c <= '9':
			r |= rune(c - '0')
		case c >= 'a' && c <= 'f':
			r |= rune(c - 'a' + 10)
		default:
			r |= rune(c - 'A' + 10)
		}
	}
	return r
}
