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
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestQualp: the mock QualP serves until its context ends, taking only
// the token named; a token variable that's empty, a bad flag or an address
// in use stop it.
func TestQualp(t *testing.T) {
	var stderr bytes.Buffer
	t.Setenv("QUALP_TEST_TOKEN", "")
	for name, args := range map[string][]string{
		"empty token":     {"qualp", "--token-env", "QUALP_TEST_TOKEN"},
		"an unknown flag": {"qualp", "--port", "1"},
		"a bad address":   {"qualp", "--listen", "nowhere:x"},
	} {
		if err := run(context.Background(), args, &stderr); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	t.Setenv("QUALP_TEST_TOKEN", "s3cret")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	stderr.Reset()
	go func() {
		done <- run(ctx, []string{"qualp", "--listen", addr, "--token-env", "QUALP_TEST_TOKEN"}, &stderr)
	}()
	status := func(token string) int {
		for range 50 {
			req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/tabela-frete/v1",
				strings.NewReader(`{"distance":100,"axis":2,"freight_type":"A","load_type":"granel_solido","is_empty_return":false}`))
			req.Header.Set("Access-Token", token)
			if resp, err := http.DefaultClient.Do(req); err == nil {
				_ = resp.Body.Close()
				return resp.StatusCode
			}
			time.Sleep(20 * time.Millisecond)
		}
		return 0
	}
	if got := status("s3cret"); got != http.StatusOK {
		t.Errorf("the token: HTTP %d", got)
	}
	if got := status("other"); got != http.StatusUnauthorized {
		t.Errorf("another token: HTTP %d", got)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("stopping: %v", err)
	}
	if !strings.Contains(stderr.String(), "ulpmock qualp") || strings.Contains(stderr.String(), "s3cret") {
		t.Errorf("log: %s", stderr.String())
	}
}
