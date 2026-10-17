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
	"strings"
	"testing"
)

// TestDelegationBodies: the grant's note, the subscription request and its
// answer, valid and not.
func TestDelegationBodies(t *testing.T) {
	leg := &PlannedLeg{Mode: ModeRoad, From: "BRSAO", To: "BRGRU", PlannedArrival: "2026-10-21T14:00:00Z"}
	sub := Subscription{Endpoint: "https://aurora.example/ulp", Intents: []string{TnTEvent}}
	for name, b := range map[string]Body{
		"a grant":           &DelegationGrantBody{},
		"a grant with note": &DelegationGrantBody{Note: "For your DC"},
		"a request":         &TnTSubscriptionRequestBody{Subscription: sub},
		"confirmed":         &TnTSubscriptionConfirmationBody{Status: StatusConfirmed, SubscriptionID: "SUB-D-1", ExpiresAt: "2026-11-01T00:00:00Z", Leg: leg},
		"rejected":          &TnTSubscriptionConfirmationBody{Status: StatusRejected, Messages: []Message{{Code: "full", Detail: "Full."}}},
	} {
		if err := b.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, b := range map[string]Body{
		"a long note":           &DelegationGrantBody{Note: strings.Repeat("x", MaxNote+1)},
		"a note with a newline": &DelegationGrantBody{Note: "a\nb"},
		"no endpoint":           &TnTSubscriptionRequestBody{Subscription: Subscription{Intents: []string{TnTEvent}}},
		"confirmed, no leg":     &TnTSubscriptionConfirmationBody{Status: StatusConfirmed, SubscriptionID: "SUB-D-1", ExpiresAt: "2026-11-01T00:00:00Z"},
		"confirmed, bad time":   &TnTSubscriptionConfirmationBody{Status: StatusConfirmed, SubscriptionID: "SUB-D-1", ExpiresAt: "soon", Leg: leg},
		"confirmed, bad id":     &TnTSubscriptionConfirmationBody{Status: StatusConfirmed, SubscriptionID: "a b", ExpiresAt: "2026-11-01T00:00:00Z", Leg: leg},
		"confirmed, bad leg":    &TnTSubscriptionConfirmationBody{Status: StatusConfirmed, SubscriptionID: "SUB-D-1", ExpiresAt: "2026-11-01T00:00:00Z", Leg: &PlannedLeg{Mode: ModeRoad, From: "x"}},
		"rejected, no reason":   &TnTSubscriptionConfirmationBody{Status: StatusRejected},
		"rejected, subscribed":  &TnTSubscriptionConfirmationBody{Status: StatusRejected, SubscriptionID: "SUB-D-1", Messages: []Message{{Code: "full", Detail: "Full."}}},
		"another status":        &TnTSubscriptionConfirmationBody{Status: "pending"},
	} {
		if err := b.Validate(); err == nil {
			t.Errorf("%s: valid", name)
		}
	}
	for intent, b := range map[string]Body{DelegationGrant: &DelegationGrantBody{}, TnTSubscriptionRequest: &TnTSubscriptionRequestBody{},
		TnTSubscriptionConfirmation: &TnTSubscriptionConfirmationBody{}} {
		if b.Intent() != intent {
			t.Errorf("%T.Intent() = %s", b, b.Intent())
		}
	}
}
