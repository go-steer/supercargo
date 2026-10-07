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

package shipper

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/go-steer/supercargo/internal/business"
	"github.com/go-steer/supercargo/internal/llm"
	"github.com/go-steer/supercargo/internal/llm/fake"
	"github.com/go-steer/supercargo/pkg/intents"
)

// FakeSummaryModel is the fake model (llm.provider: fake) for the alert
// summarizer: it reads the prompt's blocks, as a model would, and writes
// the template's notification in the reply format the summarizer asks
// for. Rehearsals and CI get a grounded notification with no API key,
// through the same validation path a real model's goes.
func FakeSummaryModel() *fake.Model {
	return &fake.Model{Reply: func(req llm.GenerateRequest) (string, error) {
		if len(req.Messages) == 0 {
			return "", errors.New("fake: no message")
		}
		msg := req.Messages[len(req.Messages)-1].Text
		var b bookingFacts
		var alert intents.TnTAlertBody
		var biz struct {
			Shipment *shipmentView `json:"shipment"`
		}
		var facts impactFacts
		for name, v := range map[string]any{"booking": &b, "alert": &alert, "business": &biz, "impact": &facts} {
			if err := json.Unmarshal([]byte(block(msg, name)), v); err != nil {
				return "", errors.New("fake: the " + name + " block: " + err.Error())
			}
		}
		var ship *business.Shipment
		if biz.Shipment != nil {
			s := biz.Shipment.shipment()
			ship = &s
		}
		text, cited := templateNotification(b, &alert, alert.CauseEventIDs, ship, facts)
		out, err := json.Marshal(map[string]any{"notification": text, "citedEventIds": cited})
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
