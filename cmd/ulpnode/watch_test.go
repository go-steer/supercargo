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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/activity"
	"github.com/go-steer/supercargo/internal/config"
	"github.com/go-steer/supercargo/internal/control"
)

// syncBuffer is an io.Writer two goroutines can share.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestWatch: entries stream out as JSON lines, history first, and watch
// picks up from the last one when the stream is cut.
func TestWatch(t *testing.T) {
	cfg := shipperConfig(t)
	c, _ := config.Load(cfg)
	l, err := control.Listen(c.Control.Socket)
	if err != nil {
		t.Fatal(err)
	}
	bus := activity.NewBus(0)
	bus.Publish(activity.Entry{Kind: activity.KindVoyage, Voyage: &activity.Voyage{Booking: "OB-1", Started: true}})
	srv := control.Server(activity.Handler(bus), nil)
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	var out, errOut syncBuffer
	done := make(chan error, 1)
	go func() { done <- watch(ctx, []string{"--config", cfg}, &out, &errOut) }()
	waitFor := func(n int) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if strings.Count(out.String(), "\n") >= n {
				return
			}
		}
		t.Fatalf("waited for %d lines, got:\n%s", n, out.String())
	}
	waitFor(1)
	bus.Publish(activity.Entry{Kind: activity.KindPush, Push: &activity.Push{Booking: "OB-1", Problem: "dropped", Error: "x"}})
	waitFor(2)
	cancel()
	if err := <-done; err != nil {
		t.Errorf("watch: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	var first, second activity.Entry
	_ = json.Unmarshal([]byte(lines[0]), &first)
	_ = json.Unmarshal([]byte(lines[1]), &second)
	if first.Seq != 1 || first.Voyage == nil || second.Seq != 2 || second.Push == nil {
		t.Errorf("entries %+v %+v", first, second)
	}
}

// TestWatchReconnects: when a stream is cut, watch reconnects and asks for
// the entries after the last one it printed.
func TestWatchReconnects(t *testing.T) {
	cfg := shipperConfig(t)
	c, _ := config.Load(cfg)
	l, err := control.Listen(c.Control.Socket)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var asked []string
	srv := control.Server(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.Header.Get("Last-Event-ID"))
		n := len(asked)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "id: %d\nevent: push\ndata: {\"seq\":%d,\"kind\":\"push\"}\n\n", n, n)
		w.(http.Flusher).Flush()
		if n == 1 {
			return // cut
		}
		<-r.Context().Done()
	}), nil)
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	var out, errOut syncBuffer
	done := make(chan error, 1)
	go func() { done <- watch(ctx, []string{"--config", cfg}, &out, &errOut) }()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && strings.Count(out.String(), "\n") < 2; {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(asked) < 2 || asked[0] != "" || asked[1] != "1" {
		t.Errorf("Last-Event-ID per connection: %q, want \"\" then \"1\"", asked)
	}
	if strings.Count(out.String(), "\n") < 2 {
		t.Errorf("printed:\n%s", out.String())
	}
}

func TestWatchRejects(t *testing.T) {
	cfg := shipperConfig(t)
	noControl := writeFile(t, "nc.yaml", strings.Split(mustRead(t, cfg), "control:")[0])
	for name, args := range map[string][]string{
		"no config":        {},
		"missing config":   {"--config", "/nonexistent.yaml"},
		"no control":       {"--config", noControl},
		"bad flag":         {"--nope"},
		"node not running": {"--config", cfg},
	} {
		if err := watch(context.Background(), args, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
			t.Errorf("%s: succeeded", name)
		}
	}
}
