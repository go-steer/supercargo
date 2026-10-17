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

package intents

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/go-steer/supercargo/pkg/envelope"
)

// TestDecodeDelegation: a grant and a subscription request decode with
// their token and booking, and not without; the answer decodes in reply.
func TestDecodeDelegation(t *testing.T) {
	token := json.RawMessage(`{"protected":"e30","payload":"e30","signature":"AA"}`)
	env := func(intent, body string, delegation json.RawMessage) *envelope.Envelope {
		h := envelope.NewHeader(intent, "vale-verde.example", "aurora.example", now)
		h.BookingRef = ptr("SR-BKG-0913")
		return &envelope.Envelope{Header: h, Body: json.RawMessage(body), Attachments: envelope.Attachments{Delegation: delegation}}
	}
	sub := `{"subscription":{"endpoint":"https://aurora.example/ulp","intents":["ulp.tnt.event"]}}`
	if b, err := Decode(env(DelegationGrant, `{"note":"For your DC"}`, token)); err != nil || b.(*DelegationGrantBody).Note != "For your DC" {
		t.Errorf("a grant: %v", err)
	}
	if _, err := Decode(env(TnTSubscriptionRequest, sub, token)); err != nil {
		t.Errorf("a subscription: %v", err)
	}
	for name, e := range map[string]*envelope.Envelope{
		"a grant without its token":        env(DelegationGrant, `{}`, nil),
		"a subscription without its token": env(TnTSubscriptionRequest, sub, nil),
	} {
		if _, err := Decode(e); !errors.Is(err, ErrInvalidBody) {
			t.Errorf("%s: %v", name, err)
		}
	}
	req := env(TnTSubscriptionRequest, sub, token)
	h := req.Header.Reply(TnTSubscriptionConfirmation, now)
	h.BookingRef = ptr("SR-BKG-0913")
	answer := &envelope.Envelope{Header: h, Body: json.RawMessage(`{"status":"rejected","messages":[{"code":"full","detail":"Full."}]}`)}
	if _, err := Decode(answer); err != nil {
		t.Errorf("the answer: %v", err)
	}
}
