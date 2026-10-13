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

package edi

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// The drop's moves and reads, relative to the directories NewDir holds
// open, never through a name a partner could have replaced.

func (d *Dir) fd(dir string) int { return int(d.dirs[dir].Fd()) }

// taken reports whether name exists in dir, without following it.
func (d *Dir) taken(dir, name string) (bool, error) {
	var st unix.Stat_t
	err := unix.Fstatat(d.fd(dir), name, &st, unix.AT_SYMLINK_NOFOLLOW)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, unix.ENOENT):
		return false, nil
	default:
		return false, &os.PathError{Op: "fstatat", Path: name, Err: err}
	}
}

// rename moves name in from to kept in to.
func (d *Dir) rename(from, name, to, kept string) error {
	if err := unix.Renameat(d.fd(from), name, d.fd(to), kept); err != nil {
		return &os.LinkError{Op: "renameat", Old: name, New: kept, Err: err}
	}
	return nil
}

// open opens name in dir for reading, refusing a symlink and not
// blocking on a FIFO.
func (d *Dir) open(dir, name string) (*os.File, error) {
	fd, err := unix.Openat(d.fd(dir), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "openat", Path: name, Err: err}
	}
	return os.NewFile(uintptr(fd), name), nil
}

// links is a file's number of hard links.
func links(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink) //nolint:unconvert // uint16 or uint32 on some systems
	}
	return 1
}

// private refuses a subdirectory another user owns, or that group or
// others may write to.
func private(fi os.FileInfo) error {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("owned by uid %d, not this process's", st.Uid)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("writable by group or others (%v)", fi.Mode().Perm())
	}
	return nil
}
