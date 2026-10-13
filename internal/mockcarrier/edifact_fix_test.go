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

package mockcarrier

import (
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/edi"
)

func instructionAt(pickup, latest time.Time, loc *time.Location, cargo string) []byte {
	data, err := edi.WriteIFTMIN(edi.Instruction{Reference: "MF-0007-L1", CargoDescription: cargo,
		Pickup:     edi.Stop{Name: "Cymbal warehouse", Country: "NL", UNLocode: "NLTLB", Earliest: pickup},
		Delivery:   edi.Stop{Name: "ECT Delta Terminal", Country: "NL", UNLocode: "NLRTM", Latest: latest},
		Containers: []edi.Container{{SizeType: "45G1", Count: 1, GrossWeightKg: 12000}},
	}, edi.Envelope{Sender: "meridian-forwarding.example", Recipient: "polder-haulage.example", Control: "MF000031",
		At: time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC), Location: loc})
	if err != nil {
		panic(err)
	}
	return data
}

// TestEDIFACTWritableTimes: in Amsterdam, a status that would fall in the
// hour the clocks repeat (25 October 2026) is reported an hour later, so
// the forwarder can read it.
func TestEDIFACTWritableTimes(t *testing.T) {
	ams, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Skip("no time zone data")
	}
	r := newRig(t)
	r.polder.Zone = ams
	// Picked up 00:15 UTC (02:15 summer time); departed 15 minutes later
	// would be 02:30 local, which happens twice.
	if _, err := r.toPol.Put("MF000031", instructionAt(time.Date(2026, 10, 24, 22, 15, 0, 0, time.UTC), time.Time{}, ams, "Footwear")); err != nil {
		t.Fatal(err)
	}
	r.now = time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	if err := r.polder.Step(); err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(10 * time.Second)
	if err := r.polder.Step(); err != nil {
		t.Fatal(err)
	}
	var sts []edi.Status
	if err := r.fromPol.Receive(func(_ string, data []byte) error {
		if s, err := edi.ParseIFTSTAIn(data, ams); err == nil {
			sts = append(sts, s...)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(sts) != 4 {
		t.Fatalf("%d statuses readable, want 4: %+v", len(sts), sts)
	}
	for _, s := range sts {
		if !edi.Writable(s.Time, ams) {
			t.Errorf("status %s at %s", s.Code, s.Time)
		}
	}
}

// TestEDIFACTOrder: a delivery deadline before the truck could arrive is
// missed, never reported before the pick-up.
func TestEDIFACTOrder(t *testing.T) {
	r := newRig(t)
	pickup := time.Date(2026, 10, 21, 7, 0, 0, 0, time.UTC)
	if _, err := r.toPol.Put("MF000031", instructionAt(pickup, pickup.Add(10*time.Minute), nil, "Footwear")); err != nil {
		t.Fatal(err)
	}
	if err := r.polder.Step(); err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(time.Hour)
	if err := r.polder.Step(); err != nil {
		t.Fatal(err)
	}
	_, sts := r.replies(t)
	want := []edi.Meaning{edi.PickedUp, edi.Departed, edi.GateIn, edi.Delivered}
	if len(sts) != 4 {
		t.Fatalf("statuses %+v", sts)
	}
	for n, s := range sts {
		if s.Meaning != want[n] {
			t.Errorf("status %d is %s, want %s", n, s.Meaning, want[n])
		}
	}
}

// TestEDIFACTChangedInstruction: a different trip under a reference
// already taken isn't taken.
func TestEDIFACTChangedInstruction(t *testing.T) {
	r := newRig(t)
	pickup := time.Date(2026, 10, 21, 7, 0, 0, 0, time.UTC)
	_, _ = r.toPol.Put("MF000031", instructionAt(pickup, time.Time{}, nil, "Footwear"))
	if err := r.polder.Step(); err != nil {
		t.Fatal(err)
	}
	_, _ = r.toPol.Put("MF000032", instructionAt(pickup, time.Time{}, nil, "Boots"))
	if err := r.polder.Step(); err != nil {
		t.Fatal(err)
	}
	acks, _ := r.replies(t)
	if len(acks) != 2 || !acks[0].Accepted || acks[1].Accepted || acks[1].Errors[0] != "DUP" || len(r.polder.trips) != 1 {
		t.Errorf("acks %+v, %d trips", acks, len(r.polder.trips))
	}
}

// TestEDIFACTDefaults: a carrier without a clock or a log uses the real
// clock and no log.
func TestEDIFACTDefaults(t *testing.T) {
	r := newRig(t)
	r.polder.Now, r.polder.Log = nil, nil
	if err := r.polder.Step(); err != nil {
		t.Fatal(err)
	}
	r.polder.acked = map[string]taken{}
	for n := range MaxTaken {
		r.polder.acked[string(rune(n))] = taken{}
	}
	_, _ = r.toPol.Put("MF000031", instructionAt(time.Now().Add(time.Hour), time.Time{}, nil, "Footwear"))
	_ = r.polder.Step()
	if acks, _ := r.replies(t); len(acks) != 0 {
		t.Error("past the cap: taken")
	}
}
