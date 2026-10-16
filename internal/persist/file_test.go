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

//go:build unix

package persist

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFile: the file and its WAL are the owner's alone; a second Open of
// the same file is refused while the first holds it; a path with URI
// characters in it is that path; DeleteBefore removes a range.
func TestFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a#b?c%41.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	b := d.Bucket("r")
	for _, k := range []string{"001 x", "002 y", "003 z"} {
		if err := b.Put(k, 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.DeleteBefore("003"); err != nil {
		t.Fatal(err)
	}
	var left []string
	_ = b.Load(func(k string, _ []byte) error { left = append(left, k); return nil })
	if strings.Join(left, ",") != "003 z" {
		t.Errorf("left %v", left)
	}
	for _, f := range []string{path, path + "-wal", path + ".lock"} {
		st, err := os.Stat(f)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if st.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s is %v", filepath.Base(f), st.Mode().Perm())
		}
	}
	if _, err := Open(path); !errors.Is(err, ErrLocked) {
		t.Errorf("opened twice: %v", err)
	}
	_ = d.Close()
	if _, err := b.Put("k", 1), b.DeleteBefore("z"); err == nil {
		t.Error("deleted from a closed file")
	}
	again, err := Open(path)
	if err != nil {
		t.Fatalf("opened after the first closed: %v", err)
	}
	_ = again.Close()
	// An existing file wider than the owner's is narrowed.
	wide := filepath.Join(dir, "wide.db")
	if err := os.WriteFile(wide, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := Open(wide)
	if err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	if st, _ := os.Stat(wide); st.Mode().Perm() != 0o600 {
		t.Errorf("an existing file left %v", st.Mode().Perm())
	}
	// A lock file that can't be created, or a file that can't be.
	if _, err := Open(filepath.Join(dir, "missing", "x.db")); err == nil {
		t.Error("opened in a directory that isn't there")
	}
	ro := filepath.Join(dir, "ro")
	_ = os.Mkdir(ro, 0o700)
	_ = os.WriteFile(filepath.Join(ro, "x.db.lock"), nil, 0o600)
	_ = os.Chmod(ro, 0o500)
	defer func() { _ = os.Chmod(ro, 0o700) }()
	if os.Geteuid() != 0 {
		if _, err := Open(filepath.Join(ro, "x.db")); err == nil {
			t.Error("created a file in a read-only directory")
		}
	}
}
