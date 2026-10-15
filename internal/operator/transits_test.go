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
	"context"
	"testing"
	"time"

	"github.com/go-steer/supercargo/pkg/intents"
)

// TestTransits: a port of discharge with its own transit is planned by it;
// any other by the voyage's.
func TestTransits(t *testing.T) {
	s := service()
	s.Voyage.Transits = map[string]time.Duration{"BRSSZ": 380 * time.Hour}
	for port, want := range map[string]time.Duration{"BRSSZ": 380 * time.Hour, "USEWR": 134 * time.Hour} {
		b := body()
		b.PortOfDischarge = port
		reply, err := s.Handle(context.Background(), request("cymbal-shoes.example", b))
		if err != nil {
			t.Fatal(err)
		}
		c := reply.Body.(*intents.BookingConfirmationBody)
		dep, _ := time.Parse(time.RFC3339, c.PlannedDeparture)
		arr, _ := time.Parse(time.RFC3339, c.PlannedArrival)
		if arr.Sub(dep) != want {
			t.Errorf("%s: %v, want %v", port, arr.Sub(dep), want)
		}
	}
}
