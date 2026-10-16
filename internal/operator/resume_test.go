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

	"github.com/go-steer/supercargo/internal/backend"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/intents"
)

// TestResume: after a restart, references and container serials number on
// after the highest held under the backend's own prefixes; others' are
// ignored.
func TestResume(t *testing.T) {
	held := []store.Booking{
		{Ref: "OB-BKG-1050", Confirmation: intents.BookingConfirmationBody{EBL: &intents.EBL{Containers: []intents.ShippingContainer{
			{ContainerNumber: containerNumber("OCBU", 7)}, {ContainerNumber: "MSKU1234565"}}}}},
		{Ref: "OB-BKG-1044"},
		{Ref: "OB-BKG-R-0920", Road: &store.RoadBooking{}},
		{Ref: "XX-9999"},
	}
	v := &VoyageBookings{RefPrefix: "OB-BKG", ContainerPrefix: "OCBU", Voyage: Voyage{Vessel: "V", Number: "1W", DepartureHour: 18, Transit: 1}}
	v.Resume(held)
	c, err := v.Decide(context.Background(), backend.BookingRequest{Body: &intents.BookingRequestBody{EarliestDeparture: "2026-10-20",
		Equipment: []intents.Equipment{{ISOType: "45G1", Count: 1}}}})
	if err != nil || c.BookingRef != "OB-BKG-1051" || c.EquipmentRefs()[0] != containerNumber("OCBU", 8) {
		t.Errorf("ocean: %+v, %v", c, err)
	}
	tr := trucks()
	tr.RefPrefix = "OB-BKG-R"
	tr.Resume(held)
	if r, err := tr.Decide(context.Background(), backend.RoadBookingRequest{Body: roadBody()}); err != nil || r.BookingRef != "OB-BKG-R-0921" {
		t.Errorf("road: %+v, %v", r, err)
	}
	for _, tc := range []struct {
		prefix, ref string
		n           int
		ok          bool
	}{
		{"OB-BKG", "OB-BKG-1042", 1042, true},
		{"OB-BKG", "OB-BKG-R-0913", 0, false},
		{"OB-BKG", "OB-BKG-", 0, false},
		{"OB-BKG", "OB-BKG-12x", 0, false},
		{"OB-BKG", "SR-BKG-1", 0, false},
	} {
		if n, ok := refNumber(tc.prefix, tc.ref); n != tc.n || ok != tc.ok {
			t.Errorf("refNumber(%q, %q) = %d, %v", tc.prefix, tc.ref, n, ok)
		}
	}
	if _, ok := containerSerial("OCBU", "OCBU12"); ok {
		t.Error("a short container number")
	}
}

// TestSimulatorResume: after a restart, the simulator's event and alert
// IDs number on after those held.
func TestSimulatorResume(t *testing.T) {
	s := &Simulator{Publisher: &published{}}
	s.Resume([]string{"EV-000041", "AL-000050", "MF-EV-000900", "EV-12x", "XX-000999"})
	s.mu.Lock()
	id := s.nextID("EV")
	s.mu.Unlock()
	if id != "EV-000051" {
		t.Errorf("next %s", id)
	}
}
