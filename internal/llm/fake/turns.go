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
	"strings"
	"sync"

	"github.com/go-steer/supercargo/internal/llm"
)

// Turns responds with each of turns in order, then repeats the last: a
// scripted conversation, tool calls included.
func Turns(turns ...llm.GenerateResponse) *Model {
	var mu sync.Mutex
	n := 0
	return &Model{Respond: func(llm.GenerateRequest) (llm.GenerateResponse, error) {
		mu.Lock()
		defer mu.Unlock()
		if len(turns) == 0 {
			return llm.GenerateResponse{}, nil
		}
		t := turns[min(n, len(turns)-1)]
		n++
		return t, nil
	}}
}

// Call is a tool call with its input given as JSON text.
func Call(id, name, input string) llm.ToolCall {
	return llm.ToolCall{ID: id, Name: name, Input: []byte(input)}
}

// AdversarialCalls are tool calls a confused or manipulated model might
// make. A tool loop must refuse every one before running anything. They
// assume a tool "get_events" whose input is an object with a required
// string "bookingRef" (maxLength 64) and additionalProperties false.
var AdversarialCalls = map[string]llm.ToolCall{
	"unknown tool":      Call("c1", "send_booking", `{"draftId":"D-1"}`),
	"malformed input":   Call("c2", "get_events", `{"bookingRef":`),
	"not an object":     Call("c3", "get_events", `["OB-BKG-1042"]`),
	"missing required":  Call("c4", "get_events", `{}`),
	"wrong type":        Call("c5", "get_events", `{"bookingRef":1042}`),
	"extra member":      Call("c6", "get_events", `{"bookingRef":"OB-BKG-1042","operator":"mallory.example"}`),
	"duplicate member":  Call("c7", "get_events", `{"bookingRef":"OB-BKG-1042","bookingRef":"OB-BKG-9999"}`),
	"oversized value":   Call("c8", "get_events", `{"bookingRef":"`+strings.Repeat("A", 65)+`"}`),
	"trailing data":     Call("c9", "get_events", `{"bookingRef":"OB-BKG-1042"} {"x":1}`),
	"case-variant name": Call("c10", "Get_Events", `{"bookingRef":"OB-BKG-1042"}`),
	"null input":        Call("c11", "get_events", `null`),
}
