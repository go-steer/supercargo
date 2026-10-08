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

package activity

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var errStop = errors.New("stop")

// TestStreamRoundTrip: history first, then live entries, through the SSE
// handler and the client.
func TestStreamRoundTrip(t *testing.T) {
	b := NewBus(0)
	b.Publish(envelope("a"))
	b.Publish(envelope("b"))
	srv := httptest.NewServer(Handler(b))
	t.Cleanup(srv.Close)

	var got []uint64
	err := Stream(context.Background(), srv.Client(), srv.URL, 0, func(e Entry, err error) error {
		if err != nil {
			t.Errorf("callback error %v", err)
		}
		got = append(got, e.Seq)
		if e.Envelope == nil || e.Kind != KindEnvelope {
			t.Errorf("entry %+v", e)
		}
		switch len(got) {
		case 2:
			go b.Publish(envelope("c")) // live, after the history
		case 3:
			return errStop
		}
		return nil
	})
	if !errors.Is(err, errStop) || len(got) != 3 || got[2] != 3 {
		t.Fatalf("Stream = %v, got %v", err, got)
	}
	// Resuming after 2 replays only 3.
	got = nil
	_ = Stream(context.Background(), srv.Client(), srv.URL, 2, func(e Entry, _ error) error {
		got = append(got, e.Seq)
		return errStop
	})
	if len(got) != 1 || got[0] != 3 {
		t.Errorf("resume from 2: %v", got)
	}
}

func TestStreamGapAndHeartbeat(t *testing.T) {
	old := Heartbeat
	Heartbeat = 10 * time.Millisecond
	t.Cleanup(func() { Heartbeat = old })
	b := NewBus(1)
	b.Publish(envelope("a"))
	b.Publish(envelope("b"))
	b.Publish(envelope("c"))
	srv := httptest.NewServer(Handler(b))
	t.Cleanup(srv.Close)
	var gaps, entries int
	_ = Stream(context.Background(), srv.Client(), srv.URL, 1, func(_ Entry, err error) error {
		if errors.Is(err, ErrGap) {
			gaps++
			return nil
		}
		entries++
		return errStop
	})
	if gaps != 1 || entries != 1 {
		t.Errorf("gaps %d, entries %d", gaps, entries)
	}
	// An idle stream gets heartbeats.
	resp, err := srv.Client().Get(srv.URL + "?since=2")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var seen strings.Builder
	buf := make([]byte, 256)
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline) && !strings.Contains(seen.String(), ": heartbeat"); {
		n, err := resp.Body.Read(buf)
		seen.Write(buf[:n])
		if err != nil {
			break
		}
	}
	if !strings.Contains(seen.String(), ": connected") || !strings.Contains(seen.String(), ": heartbeat") {
		t.Errorf("idle stream sent %q, want connected then a heartbeat", seen.String())
	}
}

// TestShutdownEndsStreams: closing the bus ends open streams, so the
// server serving them can shut down.
func TestShutdownEndsStreams(t *testing.T) {
	b := NewBus(0)
	srv := httptest.NewServer(Handler(b))
	t.Cleanup(srv.Close)
	done := make(chan error, 1)
	go func() {
		done <- Stream(context.Background(), srv.Client(), srv.URL, 0, func(Entry, error) error { return nil })
	}()
	time.Sleep(50 * time.Millisecond) // let it connect
	b.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("a stream ended by shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close didn't end the stream")
	}
	// After Close: history only, then the stream ends.
	b.Publish(envelope("late"))
	var got int
	if err := Stream(context.Background(), srv.Client(), srv.URL, 0, func(Entry, error) error { got++; return nil }); err != nil || got != 1 {
		t.Errorf("after Close: %d entries, %v", got, err)
	}
}

// TestStreamSides: ?side= keeps that side's entries and the whole node's,
// in the history and live.
func TestStreamSides(t *testing.T) {
	b := NewBus(0)
	side := func(s, id string) Entry { e := envelope(id); e.Side = s; return e }
	b.Publish(side(SideOperator, "op"))
	b.Publish(side(SideCustomer, "cu"))
	b.Publish(side("", "node"))
	srv := httptest.NewServer(Handler(b))
	t.Cleanup(srv.Close)
	var got []string
	err := Stream(context.Background(), srv.Client(), srv.URL+"?side=customer", 0, func(e Entry, _ error) error {
		got = append(got, e.Envelope.MessageID)
		switch len(got) {
		case 2:
			go func() { b.Publish(side(SideOperator, "op-live")); b.Publish(side(SideCustomer, "cu-live")) }()
		case 3:
			return errStop
		}
		return nil
	})
	if !errors.Is(err, errStop) || strings.Join(got, ",") != "cu,node,cu-live" {
		t.Errorf("customer stream: %v, %v", got, err)
	}
	if !(Entry{Side: SideOperator}).Visible(nil) || (Entry{Side: SideOperator}).Visible([]string{SideCustomer}) {
		t.Error("Visible")
	}
}

func TestHandlerRejects(t *testing.T) {
	h := Handler(NewBus(0))
	for name, req := range map[string]*http.Request{
		"POST":      httptest.NewRequest(http.MethodPost, "/events", nil),
		"bad since": httptest.NewRequest(http.MethodGet, "/events?since=x", nil),
		"bad side":  httptest.NewRequest(http.MethodGet, "/events?side=everyone", nil),
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			t.Errorf("%s: HTTP 200", name)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/events", nil)
	req.Header.Set("Last-Event-ID", "nope")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad Last-Event-ID: HTTP %d", rec.Code)
	}
}

func TestStreamErrors(t *testing.T) {
	serve := func(h http.HandlerFunc) string {
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		return srv.URL
	}
	nop := func(Entry, error) error { return nil }
	for name, url := range map[string]string{
		"not 200": serve(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }),
		"not SSE": serve(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("hi")) }),
		"bad entry": serve(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("event: envelope\ndata: {nope\n\n"))
		}),
		"oversized event": serve(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: " + strings.Repeat("x", MaxEvent+1) + "\n\n"))
		}),
	} {
		if err := Stream(context.Background(), http.DefaultClient, url, 0, nop); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if err := Stream(context.Background(), http.DefaultClient, "http://127.0.0.1:1", 0, nop); err == nil {
		t.Error("nothing listening: no error")
	}
	if err := Stream(context.Background(), http.DefaultClient, "::bad", 0, nop); err == nil {
		t.Error("a bad URL: no error")
	}
	// A stream that just ends is not an error: the caller reconnects.
	ended := serve(func(w http.ResponseWriter, _ *http.Request) { w.Header().Set("Content-Type", "text/event-stream") })
	if err := Stream(context.Background(), http.DefaultClient, ended, 0, nop); err != nil {
		t.Errorf("an ended stream: %v", err)
	}
}
