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

package llm_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/go-steer/supercargo/internal/llm"
	"github.com/go-steer/supercargo/internal/llm/fake"
)

var tools = []llm.Tool{
	{Name: "get_events", Description: "A booking's events.", Schema: []byte(`{"type":"object","properties":{
		"bookingRef":{"type":"string","maxLength":64}},"required":["bookingRef"],"additionalProperties":false}`)},
	{Name: "draft_booking", Schema: []byte(`{"type":"object","properties":{
		"count":{"type":"integer","minimum":1,"maximum":50},
		"weight":{"type":"number","maximum":30000},
		"hazardous":{"type":"boolean"},
		"label":{"type":"string","enum":["20GP","40HC"]},
		"notes":{"type":"array","maxItems":2,"items":{"type":"string","maxLength":10}},
		"origin":{"type":"object","properties":{"unLocode":{"type":"string"}},"required":["unLocode"],"additionalProperties":false}
	},"required":["count"]}`)},
	{Name: "broken", Schema: []byte(`{`)},
	{Name: "exotic", Schema: []byte(`{"type":"object","properties":{"x":{"type":"null"}}}`)},
}

func TestValidateCallAccepts(t *testing.T) {
	for _, c := range []llm.ToolCall{
		fake.Call("1", "get_events", `{"bookingRef":"OB-BKG-1042"}`),
		fake.Call("2", "draft_booking", `{"count":2,"weight":12000.5,"hazardous":false,"label":"40HC","notes":["fragile"],"origin":{"unLocode":"NLRTM"},"other":1}`),
		fake.Call("3", "draft_booking", `{"count":50}`),
		// A client holding every number as a float (Gemini Enterprise's).
		fake.Call("4", "draft_booking", `{"count":1.0}`),
		fake.Call("5", "draft_booking", `{"count":12.000,"weight":12000.0}`),
	} {
		if err := llm.ValidateCall(tools, c); err != nil {
			t.Errorf("%s %s: %v", c.Name, c.Input, err)
		}
	}
}

// TestValidateCallRefusesAdversarial: every adversarial call is refused.
func TestValidateCallRefusesAdversarial(t *testing.T) {
	for name, c := range fake.AdversarialCalls {
		err := llm.ValidateCall(tools, c)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if !errors.Is(err, llm.ErrUnknownTool) && !errors.Is(err, llm.ErrBadInput) {
			t.Errorf("%s: %v, want ErrUnknownTool or ErrBadInput", name, err)
		}
	}
}

func TestValidateCallSchemaRules(t *testing.T) {
	for name, input := range map[string]string{
		"integer below minimum":             `{"count":0}`,
		"integer above maximum":             `{"count":51}`,
		"not an integer":                    `{"count":2.5}`,
		"a near-integer":                    `{"count":1.0000000000000001}`,
		"a trailing dot":                    `{"count":1.}`,
		"an exponent":                       `{"count":1e0}`,
		"an integral float out of range":    `{"count":51.0}`,
		"an exponent after a zero fraction": `{"count":1.0e0}`,
		"negative zero":                     `{"count":-0}`,
		"negative zero, float":              `{"count":-0.0}`,
		"an integral float past int64":      `{"count":18446744073709551617.0}`,
		"number too large":                  `{"count":1,"weight":30001}`,
		"number as string":                  `{"count":"2"}`,
		"boolean as string":                 `{"count":1,"hazardous":"no"}`,
		"not in enum":                       `{"count":1,"label":"45G1"}`,
		"array too long":                    `{"count":1,"notes":["a","b","c"]}`,
		"array item too long":               `{"count":1,"notes":["0123456789A"]}`,
		"array item wrong type":             `{"count":1,"notes":[1]}`,
		"not an array":                      `{"count":1,"notes":"a"}`,
		"nested extra member":               `{"count":1,"origin":{"unLocode":"NLRTM","x":1}}`,
		"nested missing":                    `{"count":1,"origin":{}}`,
		"nested not an object":              `{"count":1,"origin":"NLRTM"}`,
		"nested duplicate":                  `{"count":1,"origin":{"unLocode":"A","unLocode":"B"}}`,
		"oversized input":                   `{"count":1,"notes":["` + strings.Repeat("x", llm.MaxToolInput) + `"]}`,
		"number out of range":               `{"count":1e400}`,
		// encoding/json would read these into the "count" or "label" field,
		// past the checks on the exact name.
		"case variant":      `{"count":1,"Count":999}`,
		"case variant only": `{"COUNT":999}`,
		"long s duplicate":  "{\"count\":1,\"\u017fx\":1,\"sx\":2}",
		"invalid token":     `{"count":tru}`,
	} {
		if err := llm.ValidateCall(tools, fake.Call("x", "draft_booking", input)); !errors.Is(err, llm.ErrBadInput) {
			t.Errorf("%s: %v, want ErrBadInput", name, err)
		}
	}
	// Schemas this package can't enforce fail closed, as the tool's fault.
	for name, schema := range map[string]string{
		"broken JSON":            `{`,
		"unsupported type":       `{"type":"object","properties":{"x":{"type":"null"}}}`,
		"unsupported item type":  `{"type":"object","properties":{"x":{"type":"array","items":{"type":"null"}}}}`,
		"no root type":           `{"properties":{"count":{"type":"integer"}},"required":["count"],"additionalProperties":false}`,
		"root not an object":     `{"type":"string"}`,
		"unenforced pattern":     `{"type":"object","properties":{"s":{"type":"string","pattern":"^OB-"}}}`,
		"unenforced minLength":   `{"type":"object","properties":{"s":{"type":"string","minLength":1}}}`,
		"unenforced combinators": `{"type":"object","oneOf":[]}`,
	} {
		ts := []llm.Tool{{Name: "t", Schema: []byte(schema)}}
		if err := llm.ValidateCall(ts, fake.Call("x", "t", `{}`)); !errors.Is(err, llm.ErrBadSchema) || errors.Is(err, llm.ErrBadInput) {
			t.Errorf("%s: %v, want ErrBadSchema", name, err)
		}
	}
	// The Kelvin sign (U+212A) folds to k in encoding/json: "\u212aind" is "kind".
	kinded := []llm.Tool{{Name: "k", Schema: []byte(`{"type":"object","properties":{"kind":{"type":"string","enum":["a"]}}}`)}}
	if err := llm.ValidateCall(kinded, fake.Call("x", "k", "{\"\u212aind\":\"zzz\"}")); !errors.Is(err, llm.ErrBadInput) {
		t.Errorf("a Kelvin-sign variant: %v, want ErrBadInput", err)
	}
	// Enums compare exactly: 1 is not "1".
	untypedEnum := []llm.Tool{{Name: "e", Schema: []byte(`{"type":"object","properties":{"v":{"enum":["1",true]}}}`)}}
	for _, in := range []string{`{"v":1}`, `{"v":"true"}`} {
		if err := llm.ValidateCall(untypedEnum, fake.Call("x", "e", in)); !errors.Is(err, llm.ErrBadInput) {
			t.Errorf("enum %s: %v, want ErrBadInput", in, err)
		}
	}
	if err := llm.ValidateCall(untypedEnum, fake.Call("x", "e", `{"v":"1"}`)); err != nil {
		t.Errorf("enum exact match: %v", err)
	}
	// Annotations are accepted.
	annotated := []llm.Tool{{Name: "t", Schema: []byte(`{"$schema":"x","title":"T","description":"D","type":"object","properties":{"a":{"type":"string","description":"A"}}}`)}}
	if err := llm.ValidateCall(annotated, fake.Call("x", "t", `{"a":"b"}`)); err != nil {
		t.Errorf("annotations: %v", err)
	}
	// A property schema without a type accepts any value.
	untyped := []llm.Tool{{Name: "u", Schema: []byte(`{"type":"object","properties":{"any":{}}}`)}}
	if err := llm.ValidateCall(untyped, fake.Call("x", "u", `{"any":[1,"two",{"three":3}]}`)); err != nil {
		t.Errorf("an untyped property: %v", err)
	}
	// Duplicates inside arrays of objects are found too.
	arr := []llm.Tool{{Name: "a", Schema: []byte(`{"type":"object"}`)}}
	if err := llm.ValidateCall(arr, fake.Call("x", "a", `{"l":[{"k":1,"k":2}]}`)); !errors.Is(err, llm.ErrBadInput) {
		t.Errorf("a duplicate inside an array: %v", err)
	}
	if err := llm.ValidateCall(arr, fake.Call("x", "a", `{"l":[{"k":1},{"k":2}],"m":{"n":[]}}`)); err != nil {
		t.Errorf("the same key in sibling objects: %v", err)
	}
}

// TestInteger: integers as JSON Schema counts them, exactly, and Int reads
// what ValidateCall lets through.
func TestInteger(t *testing.T) {
	for in, want := range map[string]int64{"1": 1, "1.0": 1, "-3.000": -3, "0": 0, "999.0": 999} {
		if v, ok := llm.Integer(json.Number(in)); !ok || v != want {
			t.Errorf("Integer(%s) = %d, %v; want %d", in, v, ok, want)
		}
		var i llm.Int
		if err := json.Unmarshal([]byte(in), &i); err != nil || int64(i) != want {
			t.Errorf("Int from %s = %d, %v; want %d", in, i, err, want)
		}
	}
	for _, in := range []string{"1.5", "1.0000000000000001", "1.", "1e0", "1.0e0", "1.00E0", "", "x", `"1"`, "99999999999999999999",
		"+1", "01", "00.0", "-", "-01", ".0"} {
		if _, ok := llm.Integer(json.Number(in)); ok {
			t.Errorf("Integer(%s) accepted", in)
		}
		var i llm.Int
		if err := i.UnmarshalJSON([]byte(in)); err == nil {
			t.Errorf("Int from %s accepted", in)
		}
	}
}

// TestIntegerBounds: integer bounds compare exactly, past 2^53 and past
// what int64 holds.
func TestIntegerBounds(t *testing.T) {
	ts := []llm.Tool{
		{Name: "big", Schema: []byte(`{"type":"object","properties":{"n":{"type":"integer","maximum":9007199254740992}}}`)},
		{Name: "frac", Schema: []byte(`{"type":"object","properties":{"n":{"type":"integer","minimum":1.5,"maximum":3.5}}}`)},
		{Name: "huge", Schema: []byte(`{"type":"object","properties":{"n":{"type":"integer","minimum":-1e30,"maximum":1e30}}}`)},
		{Name: "tiny", Schema: []byte(`{"type":"object","properties":{"n":{"type":"integer","minimum":1e30}}}`)},
	}
	for _, c := range []struct {
		tool, n string
		ok      bool
	}{
		{"big", "9007199254740992", true},
		{"big", "9007199254740993", false},
		{"big", "9007199254740993.0", false},
		{"frac", "1", false},
		{"frac", "2", true},
		{"frac", "3.0", true},
		{"frac", "4", false},
		{"huge", "-9223372036854775808", true},
		{"huge", "9223372036854775807", true},
		{"tiny", "9223372036854775807", false},
	} {
		err := llm.ValidateCall(ts, fake.Call("x", c.tool, `{"n":`+c.n+`}`))
		if (err == nil) != c.ok {
			t.Errorf("%s n=%s: %v, want ok=%v", c.tool, c.n, err, c.ok)
		}
	}
}
