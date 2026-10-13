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
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
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
	for _, want := range []string{`"b.edi": not an IFTSTA`, `"c.edi":`, `"d.edi": not opened (a symlink is refused)`, `"e.edi": not a regular file`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	for dir, want := range map[string][]string{
		".":          {".partial", "done", "failed", "processing"},
		"done":       {"a.edi", "f.edi"},
		"failed":     {"b.edi", "c.edi", "d.edi", "e.edi"},
		"processing": nil,
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

// receive runs d.Receive, failing the test rather than hanging if it
// never returns.
func receive(t *testing.T, d *Dir, fn func(string, []byte) error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- d.Receive(fn) }()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("Receive did not return")
		return nil
	}
}

func listDir(t *testing.T, path string) []string {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	return got
}

// TestDirLongNames: a name as long as the file system allows, sent twice,
// is kept twice under shortened names, rather than looping forever on a
// name with ".1" that is too long.
func TestDirLongNames(t *testing.T) {
	path := t.TempDir()
	d, err := NewDir(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	long := strings.Repeat("x", 250) + ".edi" // 254 bytes
	var seen int
	for _, content := range []string{"one", "two"} {
		if err := os.WriteFile(filepath.Join(path, long), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := receive(t, d, func(name string, _ []byte) error {
			if name != long {
				t.Errorf("name %q", name)
			}
			seen++
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	short := long[:200]
	if got := listDir(t, filepath.Join(path, "done")); seen != 2 || !reflect.DeepEqual(got, []string{short, short + ".1"}) {
		t.Errorf("seen %d, done/ %v", seen, got)
	}
}

// TestDirReplacedSubdirs: a subdirectory a partner replaced with a file, a
// symlink (out of the drop or within it) or another directory stops
// Receive before anything is read or moved.
func TestDirReplacedSubdirs(t *testing.T) {
	for _, sub := range []string{"done", "failed", "processing"} {
		for kind, replace := range map[string]func(p, outside, drop string) error{
			"file":        func(p, _, _ string) error { return os.WriteFile(p, []byte("x"), 0o600) },
			"symlink out": func(p, outside, _ string) error { return os.Symlink(outside, p) },
			"symlink within": func(p, _, drop string) error {
				if err := os.Mkdir(filepath.Join(drop, ".other"), 0o700); err != nil {
					return err
				}
				return os.Symlink(".other", p)
			},
			"other directory": func(p, _, _ string) error { return os.Mkdir(p, 0o700) },
		} {
			t.Run(sub+"/"+kind, func(t *testing.T) {
				path, outside := t.TempDir(), t.TempDir()
				d, err := NewDir(path)
				if err != nil {
					t.Fatal(err)
				}
				defer d.Close()
				if err := os.WriteFile(filepath.Join(path, "a.edi"), []byte("a"), 0o600); err != nil {
					t.Fatal(err)
				}
				p := filepath.Join(path, sub)
				if err := os.RemoveAll(p); err != nil {
					t.Fatal(err)
				}
				if err := replace(p, outside, path); err != nil {
					t.Fatal(err)
				}
				called := false
				err = receive(t, d, func(string, []byte) error { called = true; return nil })
				if err == nil || called {
					t.Errorf("Receive: %v, handler called %v", err, called)
				}
				if got := listDir(t, outside); len(got) != 0 {
					t.Errorf("written outside the drop: %v", got)
				}
				if _, err := os.Lstat(filepath.Join(path, "a.edi")); err != nil {
					t.Errorf("a.edi moved: %v", err)
				}
			})
		}
	}
}

// TestDirAtMostOnce: a file whose move out of processing/ fails (done/
// swapped for a symlink while the handler ran) stays in processing/ and
// is never handed over again.
func TestDirAtMostOnce(t *testing.T) {
	path, outside := t.TempDir(), t.TempDir()
	d, err := NewDir(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := os.WriteFile(filepath.Join(path, "a.edi"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	done, away := filepath.Join(path, "done"), filepath.Join(path, ".away")
	calls := 0
	err = receive(t, d, func(string, []byte) error {
		calls++
		if err := os.Rename(done, away); err != nil {
			return err
		}
		return os.Symlink(outside, done)
	})
	if err == nil || !strings.Contains(err.Error(), "stays in processing/") {
		t.Errorf("Receive: %v", err)
	}
	if got := listDir(t, outside); len(got) != 0 {
		t.Errorf("written outside the drop: %v", got)
	}
	// While done/ is a symlink, Receive refuses to run at all.
	if err := receive(t, d, func(string, []byte) error { calls++; return nil }); err == nil {
		t.Error("Receive ran with done/ replaced")
	}
	// Put right, the drop works again, and the file isn't handed over twice.
	if err := os.Remove(done); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(away, done); err != nil {
		t.Fatal(err)
	}
	if err := receive(t, d, func(string, []byte) error { calls++; return nil }); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("handed over %d times", calls)
	}
	if got := listDir(t, filepath.Join(path, "processing")); !reflect.DeepEqual(got, []string{"a.edi"}) {
		t.Errorf("processing/ %v", got)
	}
}

// TestDirRace: a subdirectory swapped for a symlink into the drop after
// move's checks and before its rename doesn't decide where the file goes,
// so it isn't put back in the drop to be handed over again.
func TestDirRace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("moves go by name off Unix")
	}
	path := t.TempDir()
	d, err := NewDir(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, sub := range []string{"processing", "done"} {
		t.Run(sub, func(t *testing.T) {
			p, away := filepath.Join(path, sub), filepath.Join(path, ".away")
			testHookMove = func(to string) {
				if to != sub {
					return
				}
				if err := os.Rename(p, away); err != nil {
					t.Error(err)
				}
				if err := os.Symlink(".", p); err != nil {
					t.Error(err)
				}
			}
			defer func() { testHookMove = nil }()
			if err := os.WriteFile(filepath.Join(path, "a.edi"), []byte("a"), 0o600); err != nil {
				t.Fatal(err)
			}
			calls := 0
			_ = receive(t, d, func(string, []byte) error { calls++; return nil })
			testHookMove = nil
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(away, p); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(filepath.Join(path, "a.edi")); err == nil {
				t.Error("a.edi is back in the drop")
			}
			if err := receive(t, d, func(string, []byte) error { calls++; return nil }); err != nil {
				t.Error(err)
			}
			if calls != 1 {
				t.Errorf("handed over %d times", calls)
			}
			// Into the original processing/ and, unable to leave it while
			// it was replaced, kept there; or into the original done/.
			want := map[string]string{"processing": "processing", "done": "done"}[sub]
			if b, err := os.ReadFile(filepath.Join(path, want, "a.edi")); err != nil || string(b) != "a" {
				t.Errorf("%s/a.edi: %q %v", want, b, err)
			}
			_ = os.Remove(filepath.Join(path, want, "a.edi"))
		})
	}
}

// TestDirLinksAndOwners: a file with another link (a partner keeping a
// hold on it, or linking in a file of the node's) is refused, and so is a
// subdirectory others may write to.
func TestDirLinksAndOwners(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix only")
	}
	path, other := t.TempDir(), t.TempDir()
	d, err := NewDir(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	secret := filepath.Join(other, "secret")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(secret, filepath.Join(path, "a.edi")); err != nil {
		t.Skip("no hard links here:", err)
	}
	err = receive(t, d, func(string, []byte) error { t.Error("handed over"); return nil })
	if err == nil || !strings.Contains(err.Error(), "has 2 links") {
		t.Errorf("Receive: %v", err)
	}

	path = t.TempDir()
	if err := os.Mkdir(filepath.Join(path, "failed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(path, "failed"), 0o777); err != nil {
		t.Fatal(err)
	}
	if d, err := NewDir(path); err == nil || !strings.Contains(err.Error(), "writable by group or others") {
		if d != nil {
			d.Close()
		}
		t.Errorf("a world-writable failed/: %v", err)
	}
}

// TestDirClosed: a closed drop refuses work, and closing twice is fine.
func TestDirClosed(t *testing.T) {
	d, err := NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Error(err)
	}
	if _, err := d.Put("x", []byte("a")); !errors.Is(err, errClosed) {
		t.Errorf("Put: %v", err)
	}
	if err := d.Receive(func(string, []byte) error { return nil }); !errors.Is(err, errClosed) {
		t.Errorf("Receive: %v", err)
	}
}

func TestKeptName(t *testing.T) {
	for name, want := range map[string]string{
		"a.edi":                         "a.edi",
		strings.Repeat("x", 250):        strings.Repeat("x", 200),
		strings.Repeat("x", 199) + "éa": strings.Repeat("x", 199), // é would be cut in two
		strings.Repeat("x", 198) + "éa": strings.Repeat("x", 198) + "é",
	} {
		if got := keptName(name); got != want {
			t.Errorf("keptName(%d bytes) = %d bytes, want %d", len(name), len(got), len(want))
		}
	}
}

// TestDirNoFreeName: a file with no free name in processing/ is not
// handed over and stays in the drop; the next file still is.
func TestDirNoFreeName(t *testing.T) {
	path := t.TempDir()
	d, err := NewDir(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for n := range maxNameTries {
		name := "a.edi"
		if n > 0 {
			name += "." + strconv.Itoa(n)
		}
		if err := os.WriteFile(filepath.Join(path, "processing", name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"a.edi", "b.edi"} {
		if err := os.WriteFile(filepath.Join(path, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var seen []string
	err = receive(t, d, func(name string, _ []byte) error { seen = append(seen, name); return nil })
	if err == nil || !strings.Contains(err.Error(), `"a.edi" not taken: no free name`) {
		t.Errorf("Receive: %v", err)
	}
	if !reflect.DeepEqual(seen, []string{"b.edi"}) {
		t.Errorf("seen %v", seen)
	}
	if _, err := os.Lstat(filepath.Join(path, "a.edi")); err != nil {
		t.Errorf("a.edi: %v", err)
	}
}

// TestDirFailures: file-system failures are reported, not hidden, and a
// file that fails is handed over at most once.
func TestDirFailures(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions don't bind root")
	}
	// A subdirectory NewDir can't open, or can't make.
	for name, set := range map[string]func(path, outside string) error{
		"done/ a symlink out":      func(path, outside string) error { return os.Symlink(outside, filepath.Join(path, "done")) },
		"done/ a dangling symlink": func(path, _ string) error { return os.Symlink("nowhere", filepath.Join(path, "done")) },
		"read-only drop":           func(path, _ string) error { return os.Chmod(path, 0o500) },
	} {
		path, outside := t.TempDir(), t.TempDir()
		if err := set(path, outside); err != nil {
			t.Fatal(err)
		}
		if d, err := NewDir(path); err == nil {
			d.Close()
			t.Errorf("%s: opened", name)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	path := t.TempDir()
	d, err := NewDir(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, name := range []string{"a.edi", "b.edi", "c.edi"} {
		if err := os.WriteFile(filepath.Join(path, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// a.edi's handler removes b.edi, which then can't be taken; c.edi
	// can't be read.
	if err := os.Chmod(filepath.Join(path, "c.edi"), 0); err != nil {
		t.Fatal(err)
	}
	var seen []string
	err = receive(t, d, func(name string, _ []byte) error {
		seen = append(seen, name)
		return os.Remove(filepath.Join(path, "b.edi"))
	})
	if !reflect.DeepEqual(seen, []string{"a.edi"}) {
		t.Errorf("seen %v", seen)
	}
	for _, want := range []string{`"b.edi" not taken`, `"c.edi": not opened`} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Receive: %v, want %s", err, want)
		}
	}
	for dir, want := range map[string][]string{"done": {"a.edi"}, "failed": {"c.edi"}} {
		if got := listDir(t, filepath.Join(path, dir)); !reflect.DeepEqual(got, want) {
			t.Errorf("%s/ %v", dir, got)
		}
	}

	// processing/ that can't be searched: nothing is taken.
	if err := os.WriteFile(filepath.Join(path, "d.edi"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	processing := filepath.Join(path, "processing")
	if err := os.Chmod(processing, 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(processing, 0o700)
	err = receive(t, d, func(string, []byte) error { t.Error("handed over"); return nil })
	if err == nil || !strings.Contains(err.Error(), `"d.edi" not taken`) {
		t.Errorf("Receive: %v", err)
	}
}
