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

package persist

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// TestBucket: values put are loaded back, in key order, after the file is
// closed and opened again; buckets don't see each other's keys; a key put
// again is replaced, and one deleted is gone.
func TestBucket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	a, b := d.Bucket("a"), d.Bucket("b")
	for _, kv := range []struct {
		k string
		v int
	}{{"2", 2}, {"1", 1}, {"3", 3}} {
		if err := a.Put(kv.k, kv.v); err != nil {
			t.Fatal(err)
		}
	}
	_ = b.Put("1", 100)
	_ = a.Put("3", 30)
	if err := a.Delete("2"); err != nil {
		t.Fatal(err)
	}
	if err := a.Delete("nothing"); err != nil {
		t.Errorf("deleting nothing: %v", err)
	}
	_ = d.Close()

	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	var got []string
	if err := d.Bucket("a").Load(func(k string, raw []byte) error {
		var v int
		if err := json.Unmarshal(raw, &v); err != nil {
			return err
		}
		got = append(got, k+"="+string(raw))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "1=1,3=30" {
		t.Errorf("loaded %v", got)
	}
	stop := errors.New("stop")
	if err := d.Bucket("b").Load(func(string, []byte) error { return stop }); !errors.Is(err, stop) {
		t.Errorf("a loader's error: %v", err)
	}
}

// TestErrors: no file, a value that isn't JSON, and a closed file fail.
func TestErrors(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Error("opened no file")
	}
	if _, err := Open(filepath.Join(t.TempDir(), "missing", "node.db")); err == nil {
		t.Error("opened a file in a directory that isn't there")
	}
	d, err := Open(filepath.Join(t.TempDir(), "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Bucket("a").Put("k", func() {}); err == nil {
		t.Error("put a value that isn't JSON")
	}
	_ = d.Close()
	b := d.Bucket("a")
	if b.Put("k", 1) == nil || b.Delete("k") == nil || b.Load(func(string, []byte) error { return nil }) == nil {
		t.Error("a closed file worked")
	}
}
