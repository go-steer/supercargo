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

package edi

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDirPut(t *testing.T) {
	path := t.TempDir()
	d, err := NewDir(path)
	if err != nil {
		t.Fatal(err)
	}
	// A file already there is never overwritten: Put takes the next name.
	if err := os.WriteFile(filepath.Join(path, "MF-0007-L1-1.edi"), []byte("theirs"), 0o600); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, msg := range []string{"one", "two"} {
		name, err := d.Put("MF-0007-L1", []byte(msg))
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if !reflect.DeepEqual(names, []string{"MF-0007-L1-2.edi", "MF-0007-L1-3.edi"}) {
		t.Errorf("names %v", names)
	}
	for name, want := range map[string]string{"MF-0007-L1-1.edi": "theirs", "MF-0007-L1-2.edi": "one", "MF-0007-L1-3.edi": "two"} {
		p := filepath.Join(path, name)
		b, err := os.ReadFile(p)
		if err != nil || string(b) != want {
			t.Errorf("%s: %q %v", name, b, err)
		}
		if fi, _ := os.Stat(p); name != "MF-0007-L1-1.edi" && fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: mode %v", name, fi.Mode())
		}
	}
	entries, _ := os.ReadDir(path)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("temporary file %s left behind", e.Name())
		}
	}

	for name, put := range map[string]func() error{
		"bad reference": func() error { _, err := d.Put("../x", []byte("a")); return err },
		"oversized":     func() error { _, err := d.Put("x", make([]byte, MaxInterchange+1)); return err },
	} {
		if err := put(); !errors.Is(err, ErrContent) {
			t.Errorf("%s: %v, want ErrContent", name, err)
		}
	}
}

func TestDirReceive(t *testing.T) {
	path := t.TempDir()
	d, err := NewDir(path)
	if err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(path, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	write("a.edi", "first")
	write("b.edi", "refused by the handler")
	write("c.edi", strings.Repeat("x", MaxInterchange+1))
	write(".partial", "half")
	if err := os.Symlink(outside, filepath.Join(path, "d.edi")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(path, "e.edi"), 0o700); err != nil {
		t.Fatal(err)
	}
	write("f.edi", "last")

	var seen []string
	err = d.Receive(func(name string, data []byte) error {
		seen = append(seen, name+"="+string(data))
		if name == "b.edi" {
			return errors.New("not an IFTSTA")
		}
		return nil
	})
	if !reflect.DeepEqual(seen, []string{"a.edi=first", "b.edi=refused by the handler", "f.edi=last"}) {
		t.Errorf("seen %v", seen)
	}
	if err == nil {
		t.Fatal("refusals not reported")
	}
	for _, want := range []string{"b.edi: not an IFTSTA", "c.edi:", "d.edi: not a regular file", "e.edi: not a regular file"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	for dir, want := range map[string][]string{
		".":      {".partial", "done", "failed"},
		"done":   {"a.edi", "f.edi"},
		"failed": {"b.edi", "c.edi", "d.edi", "e.edi"},
	} {
		entries, _ := os.ReadDir(filepath.Join(path, dir))
		var got []string
		for _, e := range entries {
			got = append(got, e.Name())
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %v, want %v", dir, got, want)
		}
	}
	if b, _ := os.ReadFile(outside); string(b) != "secret" {
		t.Error("the symlink's target was touched")
	}

	// Read once: a second pass finds only what is new, and a name sent
	// again doesn't replace what was kept.
	write("a.edi", "again")
	seen = nil
	if err := d.Receive(func(name string, _ []byte) error { seen = append(seen, name); return nil }); err != nil || !reflect.DeepEqual(seen, []string{"a.edi"}) {
		t.Errorf("second pass: %v %v", seen, err)
	}
	for name, want := range map[string]string{"a.edi": "first", "a.edi.1": "again"} {
		if b, _ := os.ReadFile(filepath.Join(path, "done", name)); string(b) != want {
			t.Errorf("done/%s: %q, want %q", name, b, want)
		}
	}
}

func TestDirErrors(t *testing.T) {
	if _, err := NewDir(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("a missing directory opened")
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewDir(file); err == nil {
		t.Error("a file opened as a directory")
	}
	path := t.TempDir()
	if err := os.WriteFile(filepath.Join(path, "done"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewDir(path); err == nil {
		t.Error("done/ as a file accepted")
	}

	// The directory gone after opening.
	path = t.TempDir()
	d, err := NewDir(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Put("x", []byte("a")); err == nil {
		t.Error("Put into a removed directory")
	}
	if err := d.Receive(func(string, []byte) error { return nil }); err == nil {
		t.Error("Receive from a removed directory")
	}
}
