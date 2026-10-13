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
	"strings"
	"testing"
	"time"
)

// TestRun: the commands and their flags; edifact runs until its context
// ends.
func TestRun(t *testing.T) {
	var stderr bytes.Buffer
	for name, args := range map[string][]string{
		"no command":         nil,
		"unknown command":    {"x12"},
		"missing flags":      {"edifact", "--id", "a"},
		"a bad zone":         {"edifact", "--id", "a", "--partner", "b", "--in", t.TempDir(), "--out", t.TempDir(), "--zone", "Mars/Olympus"},
		"no such drop":       {"edifact", "--id", "a", "--partner", "b", "--in", "/nonexistent/x", "--out", t.TempDir()},
		"no such out drop":   {"edifact", "--id", "a", "--partner", "b", "--in", t.TempDir(), "--out", "/nonexistent/x"},
		"an unknown flag":    {"edifact", "--carrier", "a"},
		"a bad e-CMR prefix": {"edifact", "--id", "a", "--partner", "b", "--in", t.TempDir(), "--out", t.TempDir(), "--ecmr-prefix", "ECMR NL"},
		"too fast":           {"edifact", "--id", "a", "--partner", "b", "--in", t.TempDir(), "--out", t.TempDir(), "--hours-per-second", "100000"},
	} {
		if err := run(context.Background(), args, &stderr); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	stderr.Reset()
	if err := run(ctx, []string{"edifact", "--id", "polder-haulage.example", "--partner", "meridian-forwarding.example",
		"--in", t.TempDir(), "--out", t.TempDir(), "--tick", "20ms"}, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "ulpmock edifact") {
		t.Errorf("log: %s", stderr.String())
	}
}
