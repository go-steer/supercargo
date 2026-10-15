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
	"testing"

	"github.com/go-steer/supercargo/pkg/envelope"
)

// TestDecodeForwardingStatus: both intents decode under their header
// rules: a request names no booking; a response replies, naming the order
// when it found one.
func TestDecodeForwardingStatus(t *testing.T) {
	req := envelope.NewHeader(ForwardingStatusRequest, "cymbal-shoes.example", "meridian-forwarding.example", now)
	b, err := Decode(&envelope.Envelope{Header: req, Body: json.RawMessage(`{"shipper_reference":"CY-PO-55120"}`)})
	if err != nil || b.(*ForwardingStatusRequestBody).ShipperRef != "CY-PO-55120" {
		t.Errorf("request: %+v, %v", b, err)
	}
	req.BookingRef = ptr("MF-0007")
	if _, err := Decode(&envelope.Envelope{Header: req, Body: json.RawMessage(`{"shipper_reference":"CY-PO-55120"}`)}); err == nil {
		t.Error("a request naming a booking in its header")
	}
	resp := envelope.NewHeader(ForwardingStatusResponse, "meridian-forwarding.example", "cymbal-shoes.example", now)
	resp.InReplyTo = ptr("m-1")
	b, err = Decode(&envelope.Envelope{Header: resp, Body: json.RawMessage(`{"shipper_reference":"CY-PO-55120"}`)})
	if err != nil || b.(*ForwardingStatusResponseBody).Found() {
		t.Errorf("response: %+v, %v", b, err)
	}
	foundBody := json.RawMessage(`{"shipper_reference":"CY-PO-55120","forwarder_reference":"MF-0007",` +
		`"order_message_id":"0b8b7d5e-3f1a-4c2b-9d1e-5a6b7c8d9e0f","answer":{"forwarder_reference":"MF-0007","status":"pending","subscription_id":"SUB-1"}}`)
	resp.BookingRef = ptr("MF-0007")
	if _, err := Decode(&envelope.Envelope{Header: resp, Body: foundBody}); err != nil {
		t.Errorf("found: %v", err)
	}
	// The header and the body name the same order, or none.
	for name, tc := range map[string]struct {
		ref  *string
		body json.RawMessage
	}{
		"found, header names another": {ptr("MF-0008"), foundBody},
		"found, header names none":    {nil, foundBody},
		"not found, header names one": {ptr("MF-0007"), json.RawMessage(`{"shipper_reference":"CY-PO-55120"}`)},
	} {
		resp.BookingRef = tc.ref
		if _, err := Decode(&envelope.Envelope{Header: resp, Body: tc.body}); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
}
