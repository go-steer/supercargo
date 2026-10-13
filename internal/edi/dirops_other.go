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

//go:build !unix

package edi

import (
	"errors"
	"os"
	"path/filepath"
)

// The drop's moves and reads by name, through the os.Root: every file
// stays inside the drop, but a subdirectory swapped between check and
// rename decides where within it a file goes (see Dir).

func (d *Dir) taken(dir, name string) (bool, error) {
	_, err := d.root.Lstat(filepath.Join(dir, name))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

func (d *Dir) rename(from, name, to, kept string) error {
	return d.root.Rename(filepath.Join(from, name), filepath.Join(to, kept))
}

// open opens name in dir for reading, refusing a symlink (by an Lstat
// first: a swap between the two is the window the Dir doc describes).
func (d *Dir) open(dir, name string) (*os.File, error) {
	p := filepath.Join(dir, name)
	fi, err := d.root.Lstat(p)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, &os.PathError{Op: "open", Path: name, Err: errors.New("a symlink")}
	}
	return d.root.Open(p)
}

func links(os.FileInfo) uint64 { return 1 }

func private(os.FileInfo) error { return nil }
