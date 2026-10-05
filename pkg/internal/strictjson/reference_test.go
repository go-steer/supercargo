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
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"unicode/utf8"
)

// reference is a slow, obviously-correct Valid: encoding/json's tokenizer
// does all the parsing and unescaping, and this only counts depth and
// compares keys. Surrogates have no token-level view, so they are not
// compared here (TestDecodeLimits pins them).
func reference(data []byte) error {
	if !utf8.Valid(data) {
		return errors.New("utf8")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := refValue(dec, 0); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing")
	}
	return nil
}

func refValue(dec *json.Decoder, depth int) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	if depth >= MaxDepth {
		return errors.New("depth")
	}
	keys := map[string]bool{}
	for dec.More() {
		if d == '{' {
			kt, err := dec.Token()
			if err != nil {
				return err
			}
			k := kt.(string)
			if keys[k] {
				return errors.New("duplicate")
			}
			keys[k] = true
		}
		if err := refValue(dec, depth+1); err != nil {
			return err
		}
	}
	_, err = dec.Token()
	return err
}

var differentialSeeds = []string{
	`{}`, `[]`, `{"a":1,"b":{"a":2}}`, `{"a":1,"a":2}`, `{"a":1,"a":2}`,
	`{"a\"b":1,"a\"b":2}`, `{"a\\":1,"a\\\\":2}`, `{"x":"a:b","y":"\":"}`,
	`[{"k":1},{"k":1}]`, `{"k":[{"k":1}],"j":{"k":2}}`, `{"" : 1, "":2}`,
	`{"a" :1, "b"	:2, "a"
:3}`, `["a", "b", {"c": "d"}]`, `{"a":"\\","a ":1}`, `{"é":1,"é":2}`,
	`"just a string"`, `"key":`, `{"a":1}x`, `{"a":[1,2,{"b":[3,{"a":4,"a":5}]}]}`,
	strings.Repeat("[", MaxDepth) + strings.Repeat("]", MaxDepth),
	strings.Repeat("[", MaxDepth+1) + strings.Repeat("]", MaxDepth+1),
	strings.Repeat(`{"a":`, MaxDepth) + "1" + strings.Repeat("}", MaxDepth),
	strings.Repeat(`{"a":`, MaxDepth+1) + "1" + strings.Repeat("}", MaxDepth+1),
}

// TestScanMatchesReference holds the hand-written scan to the tokenizer on
// every seed: both accept, or both reject.
func TestScanMatchesReference(t *testing.T) {
	for _, s := range differentialSeeds {
		got, want := Valid([]byte(s)), reference([]byte(s))
		if (got == nil) != (want == nil) {
			t.Errorf("%q: Valid = %v, reference = %v", s, got, want)
		}
	}
}

// FuzzScanMatchesReference explores beyond the seeds:
//
//	go test -fuzz FuzzScanMatchesReference ./pkg/internal/strictjson/
func FuzzScanMatchesReference(f *testing.F) {
	for _, s := range differentialSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		got, want := Valid(data), reference(data)
		if got == nil && want != nil {
			t.Fatalf("Valid accepted what the reference rejects (%v): %q", want, data)
		}
		if got != nil && want == nil && !errors.Is(got, errLoneSurrogate) {
			t.Fatalf("Valid rejected what the reference accepts (%v): %q", got, data)
		}
	})
}
