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

package assistant

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/go-steer/supercargo/internal/shipper"
	"github.com/go-steer/supercargo/pkg/intents"
)

// TestFakeAskOrder: the fake model asks about an order by its reference
// (MF-0007 as well as OB-BKG-1042), and its report says which cited
// records came from an unsigned source, as our node marked them.
func TestFakeAskOrder(t *testing.T) {
	for _, s := range []string{"Why is MF-0007 late?", "why is OB-BKG-1042 delayed"} {
		if !askPattern.MatchString(s) || refPattern.FindString(s) == "" {
			t.Errorf("%q: not an ask about a booking", s)
		}
	}
	r, _ := json.Marshal(shipper.AskResponse{Operator: "meridian-forwarding.example",
		Answer: &intents.TnTInquiryResponseBody{Answer: "Picked up.", CitedEventIDs: []string{"MF-EV-1", "MF-EV-3"}},
		Held:   []string{"MF-EV-1", "MF-EV-3"}, Unsigned: []string{"MF-EV-1"}})
	got := askReply(string(r)).Text
	if !strings.Contains(got, "MF-EV-1 came from an EDI status nobody signed") || strings.Contains(got, "MF-EV-3 came") {
		t.Errorf("report %q", got)
	}
	r, _ = json.Marshal(shipper.AskResponse{Operator: "ocean-blue.example", Answer: &intents.TnTInquiryResponseBody{Answer: "At sea.", CitedEventIDs: []string{"EV-1"}}})
	if got := askReply(string(r)).Text; strings.Contains(got, "nobody signed") {
		t.Errorf("all signed: %q", got)
	}
}
