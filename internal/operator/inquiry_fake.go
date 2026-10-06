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

package operator

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/go-steer/supercargo/internal/llm"
	"github.com/go-steer/supercargo/internal/llm/fake"
)

// FakeInquiryModel is the fake model (llm.provider: fake) for the inquiry
// responder: it reads the records out of the prompt, as a model would, and
// answers from them like the template does, in the reply format the
// responder asks for. Rehearsals and CI get a plausible, grounded answer
// with no API key, through the same validation path a real model's goes.
func FakeInquiryModel() *fake.Model {
	return &fake.Model{Reply: func(req llm.GenerateRequest) (string, error) {
		if len(req.Messages) == 0 {
			return "", errors.New("fake: no message")
		}
		msg := req.Messages[len(req.Messages)-1].Text
		var b bookingView
		var records []recordView
		if err := json.Unmarshal([]byte(block(msg, "booking")), &b); err != nil {
			return "", err
		}
		if err := json.Unmarshal([]byte(block(msg, "records")), &records); err != nil {
			return "", err
		}
		answer, cited := templateAnswer(b, records)
		out, err := json.Marshal(map[string]any{"answer": answer, "citedEventIds": cited})
		return string(out), err
	}}
}

// block is the text between <name> and </name> in s.
func block(s, name string) string {
	_, after, ok := strings.Cut(s, "<"+name+">")
	if !ok {
		return ""
	}
	inside, _, _ := strings.Cut(after, "</"+name+">")
	return strings.TrimSpace(inside)
}
