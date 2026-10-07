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

package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/activity"
	"github.com/go-steer/supercargo/internal/config"
	"github.com/go-steer/supercargo/internal/control"
)

// TestShutdownWithStreamOpen: a connected activity stream doesn't hold up
// shutdown (http.Server.Shutdown waits for idle connections, and a stream
// never is one).
func TestShutdownWithStreamOpen(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	c := cfg(t, config.RoleOperator)
	c.Control = &config.Control{Socket: filepath.Join(dir, "ob.sock")}
	a, err := New(c, quiet())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- a.Serve(ctx) }()
	waitUnix(t, c.Control.Socket)

	streamed := make(chan error, 1)
	go func() {
		streamed <- activity.Stream(context.Background(), control.Client(c.Control.Socket, 0), "http://control/events", 0,
			func(activity.Entry, error) error { return nil })
	}()
	time.Sleep(100 * time.Millisecond) // connected
	start := time.Now()
	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve didn't return with a stream open")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("shutdown took %v with a stream open", d)
	}
	select {
	case <-streamed:
	case <-time.After(2 * time.Second):
		t.Error("the stream didn't end at shutdown")
	}
}
