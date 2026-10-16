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

// Package persist keeps a node's records across restarts in one SQLite
// file (docs/DESIGN.md, A.12 "persistence"). The stores stay in memory and
// are the source of truth while the node runs; each writes every change
// through to its bucket before it takes effect, and loads its bucket when
// the node starts. A bucket is a namespace of keys, each holding one JSON
// value. SQLite is the pure-Go driver: no cgo.
package persist

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"

	_ "modernc.org/sqlite" // the "sqlite" driver
)

// DB is one node's persistence file.
type DB struct {
	db   *sql.DB
	lock io.Closer
}

// ErrLocked reports a file another process has open as its node's.
var ErrLocked = errors.New("persist: the file is in use by another process")

// Open opens (creating if need be) the SQLite file at path. One node owns
// a file: Open takes a lock beside it (path + ".lock"), refused while
// another process holds it, and uses a single connection, in WAL mode.
// The file holds partners' bookings and messages, so it is the owner's
// alone (mode 0600, and so its WAL and shared-memory files).
func Open(path string) (*DB, error) {
	if path == "" {
		return nil, errors.New("persist: no file")
	}
	lock, err := lockFile(path + ".lock")
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*DB, error) {
		_ = lock.Close()
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fail(fmt.Errorf("persist: %w", err))
	}
	_ = f.Close()
	if err := os.Chmod(path, 0o600); err != nil {
		return fail(fmt.Errorf("persist: %w", err))
	}
	// Its WAL and shared memory, if something (a restore) made them first.
	for _, side := range []string{path + "-wal", path + "-shm"} {
		if err := os.Chmod(side, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fail(fmt.Errorf("persist: %w", err))
		}
	}
	// A URI, built so that a path with ?, # or % in it is still the path.
	dsn := (&url.URL{Scheme: "file", OmitHost: true, Path: path,
		RawQuery: "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fail(fmt.Errorf("persist: %w", err))
	}
	// One writer: the stores serialize their own changes, and SQLite
	// serializes the rest.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS kv (ns TEXT NOT NULL, key TEXT NOT NULL, value BLOB NOT NULL, PRIMARY KEY (ns, key)) WITHOUT ROWID`); err != nil {
		_ = db.Close()
		return fail(fmt.Errorf("persist: opening %s: %w", path, err))
	}
	return &DB{db: db, lock: lock}, nil
}

// Close closes the file and releases its lock.
func (d *DB) Close() error {
	err := d.db.Close()
	_ = d.lock.Close()
	return err
}

// Bucket is the namespace ns of d.
func (d *DB) Bucket(ns string) *Bucket { return &Bucket{db: d.db, ns: ns} }

// Bucket is one store's keys. It implements store.Saver.
type Bucket struct {
	db *sql.DB
	ns string
}

// Put stores v, as JSON, under key, replacing what was there.
func (b *Bucket) Put(key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("persist: %s/%s: %w", b.ns, key, err)
	}
	if _, err := b.db.Exec(`INSERT INTO kv (ns, key, value) VALUES (?, ?, ?) ON CONFLICT (ns, key) DO UPDATE SET value = excluded.value`, b.ns, key, raw); err != nil {
		return fmt.Errorf("persist: %s/%s: %w", b.ns, key, err)
	}
	return nil
}

// Delete removes key; removing one that isn't there is no error.
func (b *Bucket) Delete(key string) error {
	if _, err := b.db.Exec(`DELETE FROM kv WHERE ns = ? AND key = ?`, b.ns, key); err != nil {
		return fmt.Errorf("persist: %s/%s: %w", b.ns, key, err)
	}
	return nil
}

// DeleteBefore removes every key ordered before key, in one statement.
func (b *Bucket) DeleteBefore(key string) error {
	if _, err := b.db.Exec(`DELETE FROM kv WHERE ns = ? AND key < ?`, b.ns, key); err != nil {
		return fmt.Errorf("persist: %s before %s: %w", b.ns, key, err)
	}
	return nil
}

// Load calls fn with each key and its JSON value, in key order. It stops
// at fn's first error.
func (b *Bucket) Load(fn func(key string, raw []byte) error) error {
	rows, err := b.db.Query(`SELECT key, value FROM kv WHERE ns = ? ORDER BY key`, b.ns)
	if err != nil {
		return fmt.Errorf("persist: %s: %w", b.ns, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var key string
		var raw []byte
		if err := rows.Scan(&key, &raw); err != nil {
			return fmt.Errorf("persist: %s: %w", b.ns, err)
		}
		if err := fn(key, raw); err != nil {
			return fmt.Errorf("persist: %s/%s: %w", b.ns, key, err)
		}
	}
	return rows.Err()
}
