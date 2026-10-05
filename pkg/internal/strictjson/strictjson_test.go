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

package strictjson

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestDecode(t *testing.T) {
	v, err := Decode([]byte(" {\"n\": 1, \"f\": 1.0, \"big\": 123456789012345678901234567890, \"s\": \"\\u00e9\\ud83d\\ude00\", \"b\": true, \"z\": null, \"l\": [], \"o\": {}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"n": json.Number("1"), "f": json.Number("1.0"), "big": json.Number("123456789012345678901234567890"),
		"s": "é😀", "b": true, "z": nil, "l": []any{}, "o": map[string]any{},
	}
	if !reflect.DeepEqual(v, want) {
		t.Errorf("Decode =\n%#v\nwant\n%#v", v, want)
	}
}

func TestDecodeRejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"whitespace only", "  \n"},
		{"invalid UTF-8 in string", "\"a\xffb\""},
		{"invalid UTF-8 outside string", "[1]\xff"},
		{"duplicate key", `{"a": 1, "a": 2}`},
		{"duplicate key nested", `{"x": [{"a": 1, "b": 2, "a": 1}]}`},
		{"duplicate key after escape", `{"a": 1, "\u0061": 2}`},
		{"trailing value", `{} {}`},
		{"trailing garbage", `{}x`},
		{"unterminated object", `{"a": 1`},
		{"unterminated array", `[1, 2`},
		{"unterminated string", `"abc`},
		{"missing value", `{"a": }`},
		{"NaN literal", `[NaN]`},
		{"Infinity literal", `[Infinity]`},
		{"single quotes", `{'a': 1}`},
		{"trailing comma", `[1, 2,]`},
		{"bare control char", "\"a\x01b\""},
		{"leading zero", `[01]`},
		{"closing delimiter", `]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if v, err := Decode([]byte(tc.in)); err == nil {
				t.Errorf("Decode(%q) = %#v, want an error", tc.in, v)
			}
		})
	}
}

// TestDecodeLimits pins the bounds on unauthenticated input, nesting depth
// and lone surrogates, each just inside and just outside.
func TestDecodeLimits(t *testing.T) {
	nest := func(n int) string { return strings.Repeat("[", n) + strings.Repeat("]", n) }
	for _, tc := range []struct {
		name string
		in   string
		ok   bool
	}{
		{"depth at limit", nest(MaxDepth), true},
		{"depth over limit", nest(MaxDepth + 1), false},
		{"objects over limit", strings.Repeat(`{"a":`, MaxDepth+1) + "1" + strings.Repeat("}", MaxDepth+1), false},
		{"megabyte of brackets", strings.Repeat("[", 1<<20), false},
		{"surrogate pair", `["\ud83d\ude00"]`, true},
		{"surrogate pair uppercase", `["\uD83D\uDE00"]`, true},
		{"escaped backslash before u", `["\\ud800"]`, true},
		{"BMP escapes", `["\u00e9\uffff\u0041"]`, true},
		{"lone high", `["\ud800"]`, false},
		{"lone high then text", `["\ud800abcdefg"]`, false},
		{"lone low", `["\udc00"]`, false},
		{"reversed pair", `["\ude00\ud83d"]`, false},
		{"high then literal character", `["\ud83dA"]`, false},
		{"high then escaped BMP", `["\ud83d\u0041"]`, false},
		{"high then high", `["\ud83d\ud83d"]`, false},
		{"high then other escape", `["\ud83d\n"]`, false},
		{"lone surrogate in key", `{"\udc00": 1}`, false},
		{"lone high at end of input", `"\ud800"`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Decode([]byte(tc.in))
			if tc.ok && err != nil {
				t.Errorf("Decode: %v, want nil", err)
			}
			if !tc.ok && err == nil {
				t.Error("Decode succeeded, want an error")
			}
		})
	}
}

func TestDecodeObject(t *testing.T) {
	m, err := DecodeObject([]byte(`{"a": [1]}`))
	if err != nil || len(m) != 1 {
		t.Fatalf("DecodeObject = %v, %v", m, err)
	}
	for _, in := range []string{`[]`, `"s"`, `null`, `1`, `{"a": 1, "a": 2}`} {
		if _, err := DecodeObject([]byte(in)); err == nil {
			t.Errorf("DecodeObject(%s) succeeded, want an error", in)
		}
	}
}
