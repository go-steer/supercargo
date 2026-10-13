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

package node

import (
	"slices"
	"testing"

	"github.com/go-steer/supercargo/pkg/intents"
)

// TestSupportedDialects: a node lists ULP_ROAD only when it takes a road
// intent, so no profile claims a road capability its node lacks.
func TestSupportedDialects(t *testing.T) {
	if got := supportedDialects([]string{intents.BookingRequest, intents.TnTEvent}); slices.Contains(got, "ULP_ROAD") || !slices.Contains(got, "DCSA_TNT") {
		t.Errorf("an ocean node: %v", got)
	}
	if got := supportedDialects([]string{intents.RoadBookingRequest}); !slices.Contains(got, "ULP_ROAD") {
		t.Errorf("a road carrier: %v", got)
	}
	if got := supportedDialects([]string{intents.RoadBookingConfirmation}); !slices.Contains(got, "ULP_ROAD") {
		t.Errorf("a road customer: %v", got)
	}
}
