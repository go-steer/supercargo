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

package control

import (
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func ownerOnlyDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

type echo struct {
	Text string `json:"text"`
}

// serve runs a control server on a socket in a fresh directory, answering
// POST /echo with the request's text.
func serve(t *testing.T) string {
	t.Helper()
	path := filepath.Join(ownerOnlyDir(t), "ulp.sock")
	l, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /echo", func(w http.ResponseWriter, r *http.Request) {
		var e echo
		if err := Decode(r, &e); err != nil {
			Answer(w, http.StatusBadRequest, echo{Text: "error: " + err.Error()})
			return
		}
		Answer(w, http.StatusOK, e)
	})
	mux.HandleFunc("POST /junk", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) })
	mux.HandleFunc("POST /hangup", func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })
	srv := Server(mux, nil)
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return path
}

func TestRoundTrip(t *testing.T) {
	path := serve(t)
	c := Client(path, 5*time.Second)
	var got echo
	code, err := Call(c, "/echo", echo{Text: "hello"}, &got)
	if err != nil || code != http.StatusOK || got.Text != "hello" {
		t.Fatalf("Call = %d %+v %v", code, got, err)
	}
	// Unknown fields and other content types are refused.
	code, _ = Call(c, "/echo", map[string]any{"text": "x", "extra": 1}, &got)
	if code != http.StatusBadRequest {
		t.Errorf("unknown field: HTTP %d", code)
	}
	resp, err := c.Post("http://control/echo", "text/plain", strings.NewReader(`{"text":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("text/plain: HTTP %d", resp.StatusCode)
	}
	if _, err := Call(c, "/junk", echo{}, &got); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Errorf("a non-JSON answer: %v", err)
	}
	// Sent but unanswered: the outcome is unknown, and the error says so.
	if _, err := Call(c, "/hangup", echo{}, &got); !errors.Is(err, ErrNoAnswer) {
		t.Errorf("a dropped connection: %v, want ErrNoAnswer", err)
	}
	if _, err := Call(c, "/echo", func() {}, &got); err == nil {
		t.Error("an unmarshalable request was sent")
	}
	if _, err := Call(Client(path+".missing", time.Second), "/echo", echo{}, &got); err == nil || !strings.Contains(err.Error(), "is the node running") {
		t.Errorf("no node: %v", err)
	}
}

func TestListen(t *testing.T) {
	// A stale socket from a crashed node is replaced.
	dir := ownerOnlyDir(t)
	path := filepath.Join(dir, "ulp.sock")
	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	stale.Close()
	l, err := Listen(path)
	if err != nil {
		t.Fatalf("a stale socket wasn't replaced: %v", err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("socket mode %v, want 0600", fi.Mode().Perm())
	}
	// A live one is not stale: a second node can't take it over.
	if _, err := Listen(path); err == nil || !strings.Contains(err.Error(), "another node") {
		t.Errorf("Listen on a live socket: %v", err)
	}
	if c, err := net.Dial("unix", path); err != nil {
		t.Errorf("the first node lost its socket: %v", err)
	} else {
		c.Close()
	}
	l.Close()

	// Anything else at the path is left alone.
	file := filepath.Join(dir, "file")
	_ = os.WriteFile(file, []byte("keep"), 0o600)
	if _, err := Listen(file); err == nil {
		t.Error("Listen replaced a regular file")
	}
	if b, _ := os.ReadFile(file); string(b) != "keep" {
		t.Error("the file was changed")
	}

	// The directory must exist and be owner-only.
	open := t.TempDir()
	_ = os.Chmod(open, 0o755)
	if _, err := Listen(filepath.Join(open, "ulp.sock")); err == nil || !strings.Contains(err.Error(), "owner-only") {
		t.Errorf("a world-readable directory: %v", err)
	}
	if _, err := Listen(filepath.Join(dir, "missing", "ulp.sock")); err == nil {
		t.Error("a missing directory was accepted")
	}
	if _, err := Listen(filepath.Join(file, "ulp.sock")); err == nil {
		t.Error("a file as the directory was accepted")
	}
}
