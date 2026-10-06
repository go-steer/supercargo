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

// Package control is a node's local control API (docs/DESIGN.md, A.9):
// how the CLIs on the same host drive a running node. The shipper's books
// through it; the operator's (ulpctl) drives the simulator.
//
// It is HTTP over a Unix socket in a directory only the node's user can
// enter. Nothing about it is signed or authorized beyond that: whoever can
// reach the socket is the node's operator, the same person who can read its
// signing key. A Unix socket keeps out what a TCP port on loopback would
// let in: other users on the host, and web pages in a local browser.
package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// MaxSocketPath bounds a socket path: the OS limit is about 104 bytes.
const MaxSocketPath = 100

// Listen opens the control socket at path. Its directory must belong to
// this user and be closed to group and others; a stale socket left by a
// node that crashed is replaced, but any other file at path is an error.
func Listen(path string) (net.Listener, error) {
	dir := filepath.Dir(path)
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("control: socket directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("control: socket directory %s must be owner-only (mode %v); chmod 700 it", dir, info.Mode().Perm())
	}
	if !ownedByMe(info) {
		return nil, fmt.Errorf("control: socket directory %s must belong to this user", dir)
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode().Type() != fs.ModeSocket {
			return nil, fmt.Errorf("control: %s exists and is not a socket", path)
		}
		// Stale only if nobody answers: taking a running node's socket would
		// cut it off, and its exit would then unlink ours.
		if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
			_ = c.Close()
			return nil, fmt.Errorf("control: another node is listening on %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("control: removing a stale socket: %w", err)
		}
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("control: %w", err)
	}
	// Owner-only whatever the umask, should the directory ever not be.
	if err := os.Chmod(path, 0o600); err != nil {
		_ = l.Close()
		return nil, fmt.Errorf("control: %w", err)
	}
	return l, nil
}

// Server returns an HTTP server for h with every timeout set.
func Server(h http.Handler, errorLog *log.Logger) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      60 * time.Second, // a booking waits on the operator
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          errorLog,
	}
}

// Client returns an HTTP client that reaches the control socket at path,
// whatever host a URL names. Use "http://control/..." URLs.
func Client(path string, timeout time.Duration) *http.Client {
	var d net.Dialer
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return d.DialContext(ctx, "unix", path)
			},
		},
	}
}

// Decode reads a JSON request body into v: application/json only, size
// capped, unknown fields refused.
func Decode(r *http.Request, v any) error {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		return errors.New("content type must be application/json")
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// Answer writes v as a JSON response with status.
func Answer(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ErrNoAnswer reports a request that reached the node but got no answer
// (a timeout, a dropped connection): what it did is unknown.
var ErrNoAnswer = errors.New("control: the node didn't answer; the request may have been carried out")

// Call POSTs req as JSON to path on c and decodes the answer into resp. It
// returns the HTTP status.
func Call(c *http.Client, path string, req, resp any) (int, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return 0, err
	}
	r, err := c.Post("http://control"+path, "application/json", bytes.NewReader(body))
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return 0, fmt.Errorf("control: is the node running? %w", err)
		}
		// Sent, perhaps acted on, but unanswered.
		return 0, fmt.Errorf("%w: %w", ErrNoAnswer, err)
	}
	defer func() { _ = r.Body.Close() }()
	if err := json.NewDecoder(io.LimitReader(r.Body, 256<<10)).Decode(resp); err != nil {
		return r.StatusCode, fmt.Errorf("control: HTTP %d, unreadable answer: %w", r.StatusCode, err)
	}
	return r.StatusCode, nil
}
