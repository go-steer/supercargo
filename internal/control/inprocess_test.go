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
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestInProcess: requests reach the handler as sent, and its answer comes
// back, with nothing listening.
func TestInProcess(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, `{"got":"`+r.Method+" "+r.URL.Path+" "+string(b)+`"}`)
	})
	resp, err := InProcess(h, time.Second).Post("http://control/book", "application/json", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusTeapot || string(b) != `{"got":"POST /book x"}` {
		t.Errorf("HTTP %d %s", resp.StatusCode, b)
	}
}
