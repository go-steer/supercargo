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
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"
)

// The drop's subdirectories: where a file goes while it is handled
// (processing), and where it ends (done or failed).
const (
	processingDir = "processing"
	doneDir       = "done"
	failedDir     = "failed"
)

var subdirs = []string{processingDir, doneDir, failedDir}

// Limits on keeping a partner's file.
const (
	// maxKeptName is how much of a partner's file name is kept in a
	// subdirectory, so the ".<n>" that keeps names apart never makes one
	// too long for the file system.
	maxKeptName = 200 // bytes
	// maxNameTries bounds the search for a free name in a subdirectory.
	maxNameTries = 1000
)

// errReplaced reports a subdirectory that is no longer the directory
// NewDir found.
var errReplaced = errors.New("edi: drop directory changed")

// testHookMove, if set, runs in move after its checks and before the
// rename: the window a partner racing the receiver would use. Tests only.
var testHookMove func(to string)

// Dir is a file drop: a directory messages are written to (Put) or read
// from (Receive). A partner's side of the drop is untrusted.
//
// NewDir holds the drop and its subdirectories open, and Receive works
// relative to those handles (on Unix, renameat and openat with
// O_NOFOLLOW on the held directories), so what a partner puts at a
// subdirectory's name never decides where a file goes or what is read.
// Every other access goes through an os.Root on the drop, so no name,
// symlink or ".." reaches outside it. Receive hands each file to its
// handler at most once: it first moves the file into processing/, reads
// it there (refusing symlinks, anything but a regular file with one link,
// and anything over MaxInterchange), and then moves it to done/ or, if
// refused, to failed/. Names starting with a dot are ignored, so a
// writer's temporary files are never read half-written.
//
// The subdirectories must belong to the receiving process and not be
// writable by group or others (on Unix), and must stay the directories
// NewDir found: Receive stops if one was replaced (a file, a symlink,
// another directory), so that a person looks. A partner running as the
// receiver's own user is not defended against. On other systems the moves
// go by name through the os.Root, which keeps every file inside the drop
// but leaves a window between the check and the rename.
//
// done/ and failed/ are a record of what was received, not evidence: a
// partner holding the file open for writing could still change it. A
// partner can still rename a subdirectory away, which stops Receive until
// a person puts it back; making the drop directory sticky (chmod +t)
// stops a partner renaming what it doesn't own.
type Dir struct {
	root *os.Root
	dirs map[string]*os.File    // ".", processing, done, failed, held open
	subs map[string]os.FileInfo // each subdirectory as NewDir found it
	mu   sync.Mutex             // guards seq and closed
	seq  int
	rmu  sync.Mutex // one Receive at a time; Close takes it too
	// linked are the files the last Receive found with a second link and
	// left for one more pass (guarded by rmu).
	linked map[string]bool
	// closed is set by Close, under both locks: the raw descriptors of
	// dirs are never used after it.
	closed bool
}

// NewDir opens a file drop at path, an existing directory, creating its
// processing/, done/ and failed/ subdirectories. The caller closes it.
func NewDir(path string) (*Dir, error) {
	root, err := os.OpenRoot(path) // the node's own configuration names path
	if err != nil {
		return nil, fmt.Errorf("edi: drop directory: %w", err)
	}
	d := &Dir{root: root, dirs: map[string]*os.File{}, subs: map[string]os.FileInfo{}}
	fail := func(err error) (*Dir, error) {
		_ = d.Close() // the error to report is err
		return nil, fmt.Errorf("edi: drop directory: %w", err)
	}
	dot, err := root.Open(".")
	if err != nil {
		return fail(err)
	}
	d.dirs["."] = dot
	for _, sub := range subdirs {
		if err := root.Mkdir(sub, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return fail(err)
		}
		// Held open: the handle Receive moves files through, and so that
		// no directory made later can take its identity (inode number).
		f, err := root.Open(sub)
		if err != nil {
			return fail(err)
		}
		d.dirs[sub] = f
		fi, err := f.Stat()
		if err != nil {
			return fail(err)
		}
		d.subs[sub] = fi
		if err := d.check(sub); err != nil {
			return fail(fmt.Errorf("%s/ is not a directory", sub))
		}
		if err := private(fi); err != nil {
			return fail(fmt.Errorf("%s/: %w", sub, err))
		}
	}
	return d, nil
}

// Close releases the drop directory, after any Put or Receive under way;
// later calls fail.
func (d *Dir) Close() error {
	d.rmu.Lock()
	defer d.rmu.Unlock()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	for _, f := range d.dirs {
		_ = f.Close() // read-only directory handles
	}
	return d.root.Close()
}

var errClosed = errors.New("edi: drop directory closed")

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
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return "", errClosed
	}
	tmp := ".edi-" + rand.Text()
	f, err := d.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("edi: put: %w", err)
	}
	defer func() { _ = d.root.Remove(tmp) }() // the temporary name, linked or not
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("edi: put: %w", err)
	}
	for range 10000 {
		d.seq++
		name := fmt.Sprintf("%s-%d.edi", ref, d.seq)
		// A link, unlike a rename, fails rather than replace a file.
		err := d.root.Link(tmp, name)
		if err == nil {
			return name, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("edi: put: %w", err)
		}
	}
	return "", fmt.Errorf("edi: put: no free name for %s", ref)
}

// Receive reads each file in the drop, in name order, and hands it to fn
// at most once, with the partner's name for it (untrusted: any bytes but
// "/" and NUL; quote it before showing or logging it). A file fn accepts
// moves to done/; one fn refuses, or that is a symlink, not a regular
// file, hard-linked elsewhere or over MaxInterchange, moves to failed/. A
// file that can't be moved into processing/ is not handed to fn and stays
// where it is; one that can't be moved out of processing/ stays there,
// for a person to look at, and is never handed to fn again. The error
// joins every refusal and failure. If a subdirectory was replaced,
// Receive stops, and refuses to run until the drop is put right (the
// original directory back) or opened again. One Receive runs at a time
// per Dir; a directory has one receiving process.
func (d *Dir) Receive(fn func(name string, data []byte) error) error {
	d.rmu.Lock()
	defer d.rmu.Unlock()
	d.mu.Lock()
	closed := d.closed
	d.mu.Unlock()
	if closed {
		return errClosed
	}
	for _, sub := range subdirs {
		if err := d.check(sub); err != nil {
			return fmt.Errorf("edi: receive: %w", err)
		}
	}
	f, err := d.root.Open(".")
	if err != nil {
		return fmt.Errorf("edi: receive: %w", err)
	}
	entries, err := f.ReadDir(-1)
	_ = f.Close() // read-only
	if err != nil {
		return fmt.Errorf("edi: receive: %w", err)
	}
	slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	var errs []error
	linked := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || slices.Contains(subdirs, name) {
			continue
		}
		// A file with a second link may be a peer's Put between its link
		// and its unlink: left for the next pass. Still linked then, it is
		// refused as before (read).
		if fi, err := d.root.Lstat(name); err == nil && fi.Mode().IsRegular() && links(fi) > 1 && !d.linked[name] {
			linked[name] = true
			continue
		}
		claimed, err := d.move(".", name, processingDir)
		if err != nil {
			errs = append(errs, fmt.Errorf("edi: receive: %q not taken: %w", name, err))
			if errors.Is(err, errReplaced) {
				break
			}
			continue
		}
		data, err := d.read(name, claimed)
		if err == nil {
			if err = fn(name, data); err != nil {
				err = fmt.Errorf("edi: %q: %w", name, err)
			}
		}
		dest := doneDir
		if err != nil {
			errs = append(errs, err)
			dest = failedDir
		}
		if _, err := d.move(processingDir, claimed, dest); err != nil {
			errs = append(errs, fmt.Errorf("edi: receive: %q stays in %s/ as %q: %w", name, processingDir, claimed, err))
			if errors.Is(err, errReplaced) {
				break
			}
		}
	}
	d.linked = linked // only the files this pass left
	return errors.Join(errs...)
}

// check reports whether a subdirectory is still the real directory NewDir
// found, not a symlink, a file or another directory put in its place.
func (d *Dir) check(sub string) error {
	fi, err := d.root.Lstat(sub)
	if err != nil {
		return fmt.Errorf("%w: %w", errReplaced, err)
	}
	if !fi.IsDir() || !os.SameFile(fi, d.subs[sub]) {
		return fmt.Errorf("%w: %s/ was replaced", errReplaced, sub)
	}
	return nil
}

// move moves a file from a directory of the drop (. or a subdirectory)
// to a subdirectory, under a free name: the start of its name, then that
// and ".<n>" if it is taken, so a partner sending the same name twice
// never replaces what was kept. It returns the name in to, and gives up
// on any error but a name being taken. Only Receive writes to the
// subdirectories, one at a time, so a name free when looked up is still
// free at the rename.
func (d *Dir) move(from, name, to string) (string, error) {
	if from != "." {
		if err := d.check(from); err != nil {
			return "", err
		}
	}
	if err := d.check(to); err != nil {
		return "", err
	}
	base := keptName(name)
	for n := range maxNameTries {
		kept := base
		if n > 0 {
			kept = fmt.Sprintf("%s.%d", base, n)
		}
		taken, err := d.taken(to, kept)
		if err != nil {
			return "", err
		}
		if taken {
			continue
		}
		if testHookMove != nil {
			testHookMove(to)
		}
		if err := d.rename(from, name, to, kept); err != nil {
			return "", err
		}
		return kept, nil
	}
	return "", fmt.Errorf("no free name in %s/ after %d tries", to, maxNameTries)
}

// keptName is the start of a partner's name kept in a subdirectory: at
// most maxKeptName bytes, cut between characters.
func keptName(name string) string {
	if len(name) <= maxKeptName {
		return name
	}
	cut := maxKeptName
	for cut > 0 && !utf8.RuneStart(name[cut]) {
		cut--
	}
	return name[:cut]
}

// read reads a file in processing/, refusing a symlink (the open
// doesn't follow one) and anything but a regular file with one link
// within the cap, judged on the open file itself. name is the partner's
// name for it, for errors.
func (d *Dir) read(name, claimed string) ([]byte, error) {
	f, err := d.open(processingDir, claimed)
	if err != nil {
		return nil, fmt.Errorf("edi: %q: not opened (a symlink is refused): %w", name, err)
	}
	defer f.Close()
	ofi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("edi: %q: %w", name, err)
	}
	if !ofi.Mode().IsRegular() {
		return nil, fmt.Errorf("edi: %q: not a regular file (%s)", name, ofi.Mode().Type())
	}
	if n := links(ofi); n != 1 {
		return nil, fmt.Errorf("edi: %q: has %d links, not 1", name, n)
	}
	if ofi.Size() > MaxInterchange {
		return nil, fmt.Errorf("edi: %q: %d bytes, over %d", name, ofi.Size(), MaxInterchange)
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxInterchange+1))
	if err != nil {
		return nil, fmt.Errorf("edi: %q: %w", name, err)
	}
	if len(data) > MaxInterchange {
		return nil, fmt.Errorf("edi: %q: over %d bytes", name, MaxInterchange)
	}
	return data, nil
}
