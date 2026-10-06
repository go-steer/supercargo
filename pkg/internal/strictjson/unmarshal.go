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
	"fmt"
	"reflect"
	"strings"

	"github.com/go-steer/supercargo/pkg/internal/logsafe"
)

// Unmarshal decodes data into v (a pointer to a struct, typically) after
// checking it with Valid and checking that every object member, at every
// level, names a struct field exactly as its json tag spells it. It rejects
// members the type doesn't define.
//
// encoding/json alone matches member names case-insensitively, so for
// {"q": "a", "Q": "b"} it would hand back "b" where another implementation
// reads "a": a signed body meaning two things.
func Unmarshal(data []byte, v any) error {
	return unmarshal(data, v, false)
}

// UnmarshalKnown is Unmarshal for documents that may grow: members the type
// doesn't define are ignored, unless they differ from a defined one only in
// case, which encoding/json would silently decode into it.
func UnmarshalKnown(data []byte, v any) error {
	return unmarshal(data, v, true)
}

func unmarshal(data []byte, v any, allowUnknown bool) error {
	if err := Valid(data); err != nil {
		return err
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("strictjson: Unmarshal needs a non-nil pointer, got %T", v)
	}
	if err := checkKeys(data, rv.Type().Elem(), allowUnknown, "$"); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if !allowUnknown {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("strictjson: %w", err)
	}
	return nil
}

var unmarshalerType = reflect.TypeFor[json.Unmarshaler]()

// checkKeys walks raw alongside t. raw is valid JSON with unique keys; type
// mismatches are left for encoding/json to report.
func checkKeys(raw []byte, t reflect.Type, allowUnknown bool, path string) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if reflect.PointerTo(t).Implements(unmarshalerType) {
		return nil // decodes itself (json.RawMessage, time.Time, ...)
	}
	switch t.Kind() {
	case reflect.Struct:
		var m map[string]json.RawMessage
		if json.Unmarshal(raw, &m) != nil || m == nil {
			return nil // not an object (or null): encoding/json decides
		}
		fields := jsonFields(t, map[reflect.Type]bool{})
		for k, v := range m {
			f, ok := fields[k]
			if !ok {
				if folded := foldMatch(fields, k); folded != "" {
					return fmt.Errorf("strictjson: %s: member %s differs from %q only in case", path, logsafe.Quote(k), folded)
				}
				if allowUnknown {
					continue
				}
				return fmt.Errorf("strictjson: %s: unknown member %s", path, logsafe.Quote(k))
			}
			if err := checkKeys(v, f, allowUnknown, path+"."+k); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		var l []json.RawMessage
		if json.Unmarshal(raw, &l) != nil {
			return nil
		}
		for i, v := range l {
			if err := checkKeys(v, t.Elem(), allowUnknown, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case reflect.Map:
		var m map[string]json.RawMessage
		if json.Unmarshal(raw, &m) != nil {
			return nil
		}
		for k, v := range m {
			if err := checkKeys(v, t.Elem(), allowUnknown, path+"."+k); err != nil {
				return err
			}
		}
	}
	return nil
}

// jsonFields maps each member name encoding/json would accept for t to the
// field's type, following embedded structs.
//
// It approximates encoding/json's field rules for the shapes protocol
// bodies use: tagged fields, embedded structs (shallower names win), "-".
// It doesn't reproduce encoding/json's handling of same-depth embedded
// conflicts or invalid tag names; keep body types away from those. seen
// stops a type that embeds itself (through a pointer) from recursing
// forever.
func jsonFields(t reflect.Type, seen map[reflect.Type]bool) map[string]reflect.Type {
	fields := map[string]reflect.Type{}
	if seen[t] {
		return fields
	}
	seen[t] = true
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			et := f.Type
			for et.Kind() == reflect.Pointer {
				et = et.Elem()
			}
			if et.Kind() == reflect.Struct {
				for k, v := range jsonFields(et, seen) {
					if _, dup := fields[k]; !dup {
						fields[k] = v
					}
				}
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		fields[name] = f.Type
	}
	return fields
}

func foldMatch(fields map[string]reflect.Type, k string) string {
	for name := range fields {
		if strings.EqualFold(name, k) {
			return name
		}
	}
	return ""
}
