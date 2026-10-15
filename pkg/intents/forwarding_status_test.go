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

import "testing"

// TestForwardingStatus: a request names a reference; a response names an
// order fully, with its message's ID and an answer about it, or nothing
// but the reference.
func TestForwardingStatus(t *testing.T) {
	if err := (&ForwardingStatusRequestBody{ShipperRef: "CY-PO-55120"}).Validate(); err != nil {
		t.Errorf("request: %v", err)
	}
	if err := (&ForwardingStatusRequestBody{ShipperRef: "a/b"}).Validate(); err == nil {
		t.Error("a request for a reference that isn't one")
	}
	answer := func() *ForwardingConfirmationBody {
		return &ForwardingConfirmationBody{OrderRef: "MF-0007", Status: StatusPending, SubscriptionID: "SUB-1"}
	}
	ok := func() *ForwardingStatusResponseBody {
		return &ForwardingStatusResponseBody{ShipperRef: "CY-PO-55120", OrderRef: "MF-0007", OrderMessageID: "0b8b7d5e-3f1a-4c2b-9d1e-5a6b7c8d9e0f",
			Answer: answer()}
	}
	if err := ok().Validate(); err != nil || !ok().Found() {
		t.Errorf("found: %v", err)
	}
	if b := (&ForwardingStatusResponseBody{ShipperRef: "CY-PO-55120"}); b.Validate() != nil || b.Found() {
		t.Error("not found")
	}
	for name, edit := range map[string]func(*ForwardingStatusResponseBody){
		"no reference":             func(b *ForwardingStatusResponseBody) { b.ShipperRef = "" },
		"no order reference":       func(b *ForwardingStatusResponseBody) { b.OrderRef = "" },
		"an answer about another":  func(b *ForwardingStatusResponseBody) { b.Answer.OrderRef = "MF-0008" },
		"an invalid answer":        func(b *ForwardingStatusResponseBody) { b.Answer.Status = "maybe" },
		"no message ID":            func(b *ForwardingStatusResponseBody) { b.OrderMessageID = "" },
		"a message ID not a UUID":  func(b *ForwardingStatusResponseBody) { b.OrderMessageID = "msg-1" },
		"an uppercase message ID":  func(b *ForwardingStatusResponseBody) { b.OrderMessageID = "0B8B7D5E-3F1A-4C2B-9D1E-5A6B7C8D9E0F" },
		"a bad estimate":           func(b *ForwardingStatusResponseBody) { b.EstimatedDelivery = "soon" },
		"not found, with an order": func(b *ForwardingStatusResponseBody) { b.Answer = nil },
		"not found, with an estimate": func(b *ForwardingStatusResponseBody) {
			b.Answer, b.OrderRef, b.OrderMessageID, b.EstimatedDelivery = nil, "", "", "2026-10-26T08:00:00Z"
		},
	} {
		b := ok()
		edit(b)
		if err := b.Validate(); err == nil {
			t.Errorf("%s: valid", name)
		}
	}
	if (&ForwardingStatusRequestBody{}).Intent() != ForwardingStatusRequest || (&ForwardingStatusResponseBody{}).Intent() != ForwardingStatusResponse ||
		!Supported(ForwardingStatusRequest) || !Supported(ForwardingStatusResponse) {
		t.Error("intents")
	}
}
