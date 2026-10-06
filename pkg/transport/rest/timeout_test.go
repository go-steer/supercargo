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

package rest

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestClientTimeouts: Timeout bounds an exchange without a deadline; a
// longer context deadline is honoured up to MaxTimeout, never beyond.
func TestClientTimeouts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(150 * time.Millisecond)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	newClient := func(timeout, ceiling time.Duration) *Client {
		c, err := NewClient(profiles{doc: profileFor(t, "ocean-blue.example", "http://ocean-blue.example/ulp", true)},
			ClientOptions{AllowHTTP: true, Timeout: timeout, MaxTimeout: ceiling,
				Peers: map[string]string{"ocean-blue.example": strings.TrimPrefix(srv.URL, "http://")}})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := newClient(50*time.Millisecond, time.Second)
	if _, err := c.Post(context.Background(), "ocean-blue.example", nil); errors.Is(err, ErrNoResponse) {
		t.Error("no deadline: the default Timeout didn't apply")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Millisecond)
	defer cancel()
	if _, err := c.Post(ctx, "ocean-blue.example", nil); !errors.Is(err, ErrNoResponse) {
		t.Errorf("a longer deadline within MaxTimeout: %v", err)
	}
	capped := newClient(50*time.Millisecond, 60*time.Millisecond)
	long, cancelLong := context.WithTimeout(context.Background(), time.Hour)
	defer cancelLong()
	if _, err := capped.Post(long, "ocean-blue.example", nil); errors.Is(err, ErrNoResponse) {
		t.Error("a deadline past MaxTimeout was honoured")
	}
}
