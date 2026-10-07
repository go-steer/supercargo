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
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Heartbeat is how often an idle stream sends a comment, so proxies and
// clients can tell a quiet node from a dead connection.
var Heartbeat = 15 * time.Second

// Handler serves the activity stream as Server-Sent Events: GET with an
// optional ?since=N (or a Last-Event-ID header, as a reconnecting
// EventSource sends) replays the kept entries after N, then streams new
// ones. Each event's id is the entry's sequence number and its type the
// entry's kind; a "gap" event says entries were missed. The stream ends
// when the client goes away, or falls behind (reconnect with the last id
// seen).
func Handler(b *Bus) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		since, err := sinceOf(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		rc := http.NewResponseController(w)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)

		past, live, gap, cancel := b.Subscribe(since, 0)
		defer cancel()
		write := func(s string) bool {
			// A stream outlives the server's write timeout: each write gets
			// its own deadline instead.
			_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
			// text/event-stream, not HTML: the data lines are JSON a client parses.
			if _, err := fmt.Fprint(w, s); err != nil { //nolint:gosec // G705: not rendered as HTML
				return false
			}
			return rc.Flush() == nil
		}
		if gap && !write("event: gap\ndata: {}\n\n") {
			return
		}
		for _, e := range past {
			if !write(frame(e)) {
				return
			}
		}
		if !write(": connected\n\n") {
			return
		}
		tick := time.NewTicker(Heartbeat)
		defer tick.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case e, ok := <-live:
				if !ok {
					return // fell behind; the client reconnects with its last id
				}
				if !write(frame(e)) {
					return
				}
			case <-tick.C:
				if !write(": heartbeat\n\n") {
					return
				}
			}
		}
	})
}

func sinceOf(r *http.Request) (uint64, error) {
	s := r.URL.Query().Get("since")
	if s == "" {
		s = r.Header.Get("Last-Event-ID")
	}
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, errors.New("since must be a sequence number")
	}
	return n, nil
}

func frame(e Entry) string {
	data, _ := json.Marshal(e) // Entry always marshals
	return "id: " + strconv.FormatUint(e.Seq, 10) + "\nevent: " + e.Kind + "\ndata: " + string(data) + "\n\n"
}

// ErrGap reports, to Stream's callback, that entries were missed.
var ErrGap = errors.New("activity: entries were missed")

// MaxEvent bounds one event's data, so a broken server can't make a
// client buffer without limit.
const MaxEvent = 1 << 20

// Stream reads the activity stream at url with c, calling f with each
// entry, and with ErrGap when entries were missed. It returns when ctx is
// done, the stream ends (reconnect with since set to the last Seq seen), or
// f returns an error.
func Stream(ctx context.Context, c *http.Client, url string, since uint64, f func(Entry, error) error) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if since > 0 {
		req.Header.Set("Last-Event-ID", strconv.FormatUint(since, 10))
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return fmt.Errorf("activity: HTTP %d, content type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), MaxEvent)
	var event, data string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if event == "gap" {
				if err := f(Entry{}, ErrGap); err != nil {
					return err
				}
			} else if data != "" {
				var e Entry
				if err := json.Unmarshal([]byte(data), &e); err != nil {
					return fmt.Errorf("activity: bad entry: %w", err)
				}
				if err := f(e, nil); err != nil {
					return err
				}
			}
			event, data = "", ""
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data += strings.TrimPrefix(line, "data: ")
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return sc.Err()
}
