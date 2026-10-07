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

func TestTurns(t *testing.T) {
	m := Turns(
		llm.GenerateResponse{ToolCalls: []llm.ToolCall{Call("c1", "get_events", `{"bookingRef":"OB-1"}`)}},
		llm.GenerateResponse{Text: "Departed on the 7th."},
	)
	req := llm.GenerateRequest{Messages: []llm.Message{{Role: llm.User, Text: "Did it leave?"}}}
	r1, _ := m.Generate(context.Background(), req)
	if len(r1.ToolCalls) != 1 || r1.StopReason != llm.StopTool || r1.Model != "fake" {
		t.Errorf("turn 1: %+v", r1)
	}
	r2, _ := m.Generate(context.Background(), req)
	r3, _ := m.Generate(context.Background(), req)
	if r2.Text != "Departed on the 7th." || r2.StopReason != llm.StopEnd || r3.Text != r2.Text {
		t.Errorf("turns 2, 3: %+v %+v", r2, r3)
	}
	if r, _ := Turns().Generate(context.Background(), req); r.Text != "" || r.StopReason != llm.StopEnd {
		t.Errorf("no turns: %+v", r)
	}
	// A provider's own stop reason passes through.
	if r, _ := Turns(llm.GenerateResponse{StopReason: llm.StopMaxTokens}).Generate(context.Background(), req); r.StopReason != llm.StopMaxTokens {
		t.Errorf("stop reason %q", r.StopReason)
	}
	failing := &Model{Respond: func(llm.GenerateRequest) (llm.GenerateResponse, error) { return llm.GenerateResponse{}, ErrUnavailable }}
	if _, err := failing.Generate(context.Background(), req); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Respond error: %v", err)
	}
}
