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

package fake

import (
	"context"
	"errors"
	"testing"

	"github.com/go-steer/supercargo/internal/llm"
)

func TestScripted(t *testing.T) {
	m := Scripted("one", "two words")
	req := llm.GenerateRequest{System: "be brief", Messages: []llm.Message{{Role: llm.User, Text: "hello there"}}}
	var got []string
	for range 3 {
		r, err := m.Generate(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, r.Text)
	}
	if got[0] != "one" || got[1] != "two words" || got[2] != "two words" {
		t.Errorf("replies %v", got)
	}
	r, _ := m.Generate(context.Background(), req)
	if r.Usage.InputTokens != 4 || r.Usage.OutputTokens != 2 || r.Model != "fake" || m.Name() != "fake" {
		t.Errorf("response %+v", r)
	}
	if len(m.Calls()) != 4 {
		t.Errorf("%d calls recorded", len(m.Calls()))
	}
	if r, _ := Scripted().Generate(context.Background(), req); r.Text != "" {
		t.Errorf("empty script: %q", r.Text)
	}
	if r, _ := (&Model{}).Generate(context.Background(), req); r.Text != "" {
		t.Errorf("no Reply: %q", r.Text)
	}
}

func TestFailures(t *testing.T) {
	if _, err := Failing(ErrUnavailable).Generate(context.Background(), llm.GenerateRequest{}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Failing: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Scripted("x").Generate(ctx, llm.GenerateRequest{}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}
