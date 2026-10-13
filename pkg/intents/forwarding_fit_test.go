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

// TestFitsOrder: a confirmed journey runs from the place of receipt to the
// place of delivery, departs once the cargo is ready, and delivers in time.
func TestFitsOrder(t *testing.T) {
	req := &ForwardingRequestBody{
		PlaceOfReceipt:  Place{UNLocationCode: "NLTLB", Earliest: "2026-10-06T06:00:00Z"},
		PlaceOfDelivery: Place{UNLocationCode: "BRSAO", Latest: "2026-10-27T12:00:00Z"},
	}
	ok := &ForwardingConfirmationBody{PlannedDelivery: "2026-10-26T10:00:00Z",
		Legs: []PlannedLeg{{From: "NLTLB", To: "NLRTM", PlannedDeparture: "2026-10-06T08:00:00Z"}, {From: "NLRTM", To: "BRSAO"}}}
	if err := FitsOrder(req, ok); err != nil {
		t.Fatal(err)
	}
	for want, edit := range map[string]func(*ForwardingConfirmationBody){
		"no journey":                     func(c *ForwardingConfirmationBody) { c.Legs = nil },
		"starts at NLRTM, not the place": func(c *ForwardingConfirmationBody) { c.Legs = c.Legs[1:] },
		"ends at NLRTM, not the place":   func(c *ForwardingConfirmationBody) { c.Legs = c.Legs[:1] },
		"is after the latest asked for":  func(c *ForwardingConfirmationBody) { c.PlannedDelivery = "2026-10-28T10:00:00Z" },
		"before the cargo is ready":      func(c *ForwardingConfirmationBody) { c.Legs[0].PlannedDeparture = "2026-10-06T05:00:00Z" },
	} {
		c := *ok
		c.Legs = append([]PlannedLeg(nil), ok.Legs...)
		edit(&c)
		if err := FitsOrder(req, &c); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", want, err)
		}
	}
}
