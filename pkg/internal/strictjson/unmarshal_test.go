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
	"strings"
	"testing"
)

type inner struct {
	Name string `json:"name"`
}

type Embedded struct {
	Extra string `json:"extra"`
}

type outer struct {
	Embedded
	ShipperRef string            `json:"shipperRef"`
	Count      int               `json:"count"`
	In         inner             `json:"in"`
	Ptr        *inner            `json:"ptr"`
	List       []inner           `json:"list"`
	Map        map[string]inner  `json:"map"`
	Raw        json.RawMessage   `json:"raw"`
	Skip       string            `json:"-"`
	Untagged   string            //nolint:unused // field name is the member name
	Tags       map[string]string `json:"tags"`
	Kind       string            `json:"kind"`
	hidden     string            //nolint:unused // unexported: not a member
}

func TestUnmarshalExact(t *testing.T) {
	var o outer
	in := `{"shipperRef":"S","count":2,"in":{"name":"a"},"ptr":{"name":"b"},"list":[{"name":"c"}],` +
		`"map":{"K":{"name":"d"}},"raw":{"AnyCase":1},"extra":"e","Untagged":"u","tags":{"Any":"x"}}`
	if err := Unmarshal([]byte(in), &o); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if o.ShipperRef != "S" || o.In.Name != "a" || o.Ptr.Name != "b" || o.List[0].Name != "c" ||
		o.Map["K"].Name != "d" || o.Extra != "e" || o.Untagged != "u" || o.Tags["Any"] != "x" {
		t.Errorf("decoded %+v", o)
	}
}

func TestUnmarshalRejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
	}{
		{"case variant", `{"ShipperRef":"S"}`},
		{"case variant beside the exact one", `{"shipperRef":"S","SHIPPERREF":"T"}`},
		{"Kelvin sign folds to k", `{"\u212aind":"x"}`},
		{"long s folds to s", "{\"ſhipperRef\":\"S\"}"},
		{"nested case variant", `{"in":{"Name":"a"}}`},
		{"pointer case variant", `{"ptr":{"NAME":"a"}}`},
		{"slice element case variant", `{"list":[{"name":"a"},{"Name":"b"}]}`},
		{"map value case variant", `{"map":{"k":{"nAme":"a"}}}`},
		{"embedded case variant", `{"Extra":"e"}`},
		{"unknown member", `{"shipperRef":"S","priority":"high"}`},
		{"nested unknown member", `{"in":{"name":"a","x":1}}`},
		{"dash-tagged field", `{"-":"x"}`},
		{"field name of a dash-tagged field", `{"Skip":"x"}`},
		{"unexported field name", `{"hidden":"x"}`},
		{"duplicate key", `{"count":1,"count":2}`},
		{"type mismatch", `{"count":"2"}`},
		{"float for int", `{"count":1.5}`},
		{"trailing data", `{} {}`},
		{"invalid JSON", `{`},
		{"object field given a number", `{"in":5}`},
		{"list field given an object", `{"list":{"name":"a"}}`},
		{"map field given an array", `{"map":[{"name":"a"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var o outer
			if err := Unmarshal([]byte(tc.in), &o); err == nil {
				t.Errorf("Unmarshal(%s) succeeded: %+v", tc.in, o)
			}
		})
	}
}

func TestUnmarshalKnown(t *testing.T) {
	var o outer
	if err := UnmarshalKnown([]byte(`{"shipperRef":"S","future":{"Anything":1},"in":{"name":"a","later":2}}`), &o); err != nil {
		t.Fatalf("UnmarshalKnown ignored nothing: %v", err)
	}
	if o.ShipperRef != "S" || o.In.Name != "a" {
		t.Errorf("decoded %+v", o)
	}
	for _, in := range []string{`{"ShipperRef":"S"}`, `{"in":{"NAME":"a"}}`, `{"a":1,"a":2}`} {
		if err := UnmarshalKnown([]byte(in), &o); err == nil {
			t.Errorf("UnmarshalKnown(%s) succeeded", in)
		}
	}
}

type withPointerEmbed struct {
	*Embedded
	X string `json:"x"`
}

func TestUnmarshalEdges(t *testing.T) {
	var o outer
	if err := Unmarshal([]byte(`{"in":null,"ptr":null,"list":null,"map":null}`), &o); err != nil {
		t.Errorf("nulls for composite fields: %v", err)
	}
	var p withPointerEmbed
	if err := Unmarshal([]byte(`{"extra":"e","x":"1"}`), &p); err != nil || p.Extra != "e" {
		t.Errorf("embedded pointer struct: %+v, %v", p, err)
	}
	if err := Unmarshal([]byte(`{"Extra":"e"}`), &p); err == nil {
		t.Error("case variant of an embedded pointer struct's field accepted")
	}
}

type selfEmbed struct {
	*selfEmbed        //nolint:unused // the embedding is the point: a type that embeds itself
	Name       string `json:"name"`
}

// TestUnmarshalSelfEmbedding: a type embedding itself must not recurse
// forever.
func TestUnmarshalSelfEmbedding(t *testing.T) {
	var s selfEmbed
	if err := Unmarshal([]byte(`{"name":"a"}`), &s); err != nil || s.Name != "a" {
		t.Errorf("self-embedding type: %+v, %v", s, err)
	}
	if err := Unmarshal([]byte(`{"Name":"a"}`), &s); err == nil {
		t.Error("case variant accepted")
	}
}

func TestUnmarshalNeedsPointer(t *testing.T) {
	var o outer
	if err := Unmarshal([]byte(`{}`), o); err == nil {
		t.Error("Unmarshal into a non-pointer succeeded")
	}
	if err := Unmarshal([]byte(`{}`), (*outer)(nil)); err == nil {
		t.Error("Unmarshal into a nil pointer succeeded")
	}
}

// TestUnmarshalAgreesWithEncodingJSONOnFolding pins the premise: without
// the check, encoding/json would decode the variant into the field.
func TestUnmarshalAgreesWithEncodingJSONOnFolding(t *testing.T) {
	var kelvin outer
	if err := json.Unmarshal([]byte(`{"\u212aind":"x"}`), &kelvin); err != nil || kelvin.Kind != "x" {
		t.Error("encoding/json no longer folds the Kelvin sign into kind; the check is still defense in depth")
	}
	for _, k := range []string{"ShipperRef", "ſhipperRef"} {
		var o outer
		if err := json.Unmarshal([]byte(`{"`+k+`":"S"}`), &o); err != nil || o.ShipperRef != "S" {
			t.Errorf("encoding/json no longer folds %q into shipperRef; the check is still defense in depth", k)
		}
		if !strings.EqualFold(k, "shipperRef") {
			t.Errorf("strings.EqualFold misses %q", k)
		}
	}
}
