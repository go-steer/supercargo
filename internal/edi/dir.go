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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Dir is a file drop: a directory messages are written to (Put) or read
// from (Receive). A partner's side of the drop is untrusted: Receive reads
// each file once, refuses symlinks and anything over MaxInterchange, and
// moves what it has read to done/ or, if refused, to failed/. Names
// starting with a dot are ignored, so a writer's temporary files are never
// read half-written.
type Dir struct {
	path string
	mu   sync.Mutex // guards seq
	seq  int
	rmu  sync.Mutex // one Receive at a time
}

// NewDir opens a file drop at path, an existing directory, creating its
// done/ and failed/ subdirectories.
func NewDir(path string) (*Dir, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("edi: drop directory: %w", err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("edi: drop directory %s is not a directory", path)
	}
	for _, sub := range []string{"done", "failed"} {
		p := filepath.Join(path, sub)
		if err := os.Mkdir(p, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("edi: drop directory: %w", err)
		}
		if fi, err := os.Lstat(p); err != nil || !fi.IsDir() {
			return nil, fmt.Errorf("edi: drop directory: %s is not a directory", p)
		}
	}
	return &Dir{path: path}, nil
}

// Put writes a message as <ref>-<seq>.edi, mode 0600, atomically: a
// reader sees the whole file or none, and an existing file is never
// overwritten (the file system must support hard links). It returns the
// file's name.
func (d *Dir) Put(ref string, data []byte) (string, error) {
	if !refPattern.MatchString(ref) {
		return "", contentErr("file reference %q is not a reference", ref)
	}
	if len(data) > MaxInterchange {
		return "", contentErr("message is %d bytes, over %d", len(data), MaxInterchange)
	}
	tmp, err := os.CreateTemp(d.path, ".edi-*") // mode 0600
	if err != nil {
		return "", fmt.Errorf("edi: put: %w", err)
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("edi: put: %w", err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for range 10000 {
		d.seq++
		name := fmt.Sprintf("%s-%d.edi", ref, d.seq)
		// A link, unlike a rename, fails rather than replace a file.
		err := os.Link(tmp.Name(), filepath.Join(d.path, name))
		if err == nil {
			return name, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("edi: put: %w", err)
		}
	}
	return "", fmt.Errorf("edi: put: no free name for %s", ref)
}

// Receive reads each file in the drop, in name order, and hands it to fn.
// A file fn accepts moves to done/; one fn refuses, or that is a symlink,
// not a regular file, or over MaxInterchange, moves to failed/. Either
// way it is read only once. The error joins every refusal. One Receive
// runs at a time per Dir; a directory has one receiving process.
func (d *Dir) Receive(fn func(name string, data []byte) error) error {
	d.rmu.Lock()
	defer d.rmu.Unlock()
	entries, err := os.ReadDir(d.path)
	if err != nil {
		return fmt.Errorf("edi: receive: %w", err)
	}
	var errs []error
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || name == "done" || name == "failed" {
			continue
		}
		data, err := d.read(name)
		if err == nil {
			if err = fn(name, data); err != nil {
				err = fmt.Errorf("edi: %s: %w", name, err)
			}
		}
		dest := "done"
		if err != nil {
			errs = append(errs, err)
			dest = "failed"
		}
		if err := d.move(name, dest); err != nil {
			errs = append(errs, fmt.Errorf("edi: receive: %w", err))
		}
	}
	return errors.Join(errs...)
}

// move moves a file to done/ or failed/ under a free name, so a partner
// sending the same name twice never replaces what was kept.
func (d *Dir) move(name, dest string) error {
	to := filepath.Join(d.path, dest, name)
	for n := 1; ; n++ {
		if _, err := os.Lstat(to); errors.Is(err, os.ErrNotExist) {
			break
		}
		to = filepath.Join(d.path, dest, fmt.Sprintf("%s.%d", name, n))
	}
	return os.Rename(filepath.Join(d.path, name), to)
}

// read reads one file, refusing anything but a regular file within the
// cap, and a file swapped for another between the check and the open.
func (d *Dir) read(name string) ([]byte, error) {
	p := filepath.Join(d.path, name)
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, fmt.Errorf("edi: %s: %w", name, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("edi: %s: not a regular file (%s)", name, fi.Mode().Type())
	}
	if fi.Size() > MaxInterchange {
		return nil, fmt.Errorf("edi: %s: %d bytes, over %d", name, fi.Size(), MaxInterchange)
	}
	f, err := openNoFollow(p)
	if err != nil {
		return nil, fmt.Errorf("edi: %s: %w", name, err)
	}
	defer f.Close()
	if ofi, err := f.Stat(); err != nil || !os.SameFile(fi, ofi) {
		return nil, fmt.Errorf("edi: %s: changed while being read", name)
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxInterchange+1))
	if err != nil {
		return nil, fmt.Errorf("edi: %s: %w", name, err)
	}
	if len(data) > MaxInterchange {
		return nil, fmt.Errorf("edi: %s: over %d bytes", name, MaxInterchange)
	}
	return data, nil
}
