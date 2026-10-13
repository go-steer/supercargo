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
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/edi"
)

type rig struct {
	polder  *EDIFACT
	toPol   *edi.Dir // the forwarder's outbox, Polder's In
	fromPol *edi.Dir // Polder's Out, the forwarder's inbox
	now     time.Time
}

func newRig(t *testing.T) *rig {
	t.Helper()
	in, err := edi.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	out, err := edi.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = in.Close(); _ = out.Close() })
	r := &rig{toPol: in, fromPol: out, now: time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)}
	r.polder = &EDIFACT{ID: "polder-haulage.example", Partner: "meridian-forwarding.example", In: in, Out: out,
		ECMRPrefix: "ECMR-NL-2026", HoursPerSecond: 1, Now: func() time.Time { return r.now },
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return r
}

func instruction(sender string) []byte {
	data, err := edi.WriteIFTMIN(edi.Instruction{Reference: "MF-0007-L1", CargoDescription: "Footwear",
		Pickup:     edi.Stop{Name: "Cymbal warehouse", Street: "Ringbaan Oost 100", Locality: "Tilburg", Country: "NL", UNLocode: "NLTLB", Earliest: time.Date(2026, 10, 21, 7, 0, 0, 0, time.UTC)},
		Delivery:   edi.Stop{Name: "ECT Delta Terminal", Street: "Europaweg 875", Locality: "Rotterdam", Country: "NL", UNLocode: "NLRTM"},
		Containers: []edi.Container{{SizeType: "45G1", Count: 1, GrossWeightKg: 12000}},
	}, edi.Envelope{Sender: sender, Recipient: "polder-haulage.example", Control: "MF000031", At: time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)})
	if err != nil {
		panic(err)
	}
	return data
}

// replies collects what Polder sent: acknowledgements and statuses.
func (r *rig) replies(t *testing.T) (acks []edi.Ack, sts []edi.Status) {
	t.Helper()
	if err := r.fromPol.Receive(func(_ string, data []byte) error {
		if strings.Contains(string(data), "APERAK") {
			a, err := edi.ParseAPERAK(data)
			acks = append(acks, a)
			return err
		}
		s, err := edi.ParseIFTSTA(data)
		sts = append(sts, s...)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return acks, sts
}

// TestEDIFACTTrip: an instruction is acknowledged with an e-CMR number,
// then the trip is reported, status by status, as its clock reaches them.
func TestEDIFACTTrip(t *testing.T) {
	r := newRig(t)
	if _, err := r.toPol.Put("MF000031", instruction("meridian-forwarding.example")); err != nil {
		t.Fatal(err)
	}
	if err := r.polder.Step(); err != nil {
		t.Fatal(err)
	}
	acks, sts := r.replies(t)
	if len(acks) != 1 || !acks[0].Accepted || acks[0].Reference != "MF-0007-L1" || acks[0].TransportDocuments[0] != "ECMR-NL-2026-004471" || len(sts) != 0 {
		t.Fatalf("acks %+v, statuses %+v", acks, sts)
	}
	// The clock starts an hour before the pickup: at 1 h/s, two seconds
	// later the container is picked up and the truck has left.
	r.now = r.now.Add(2 * time.Second)
	_ = r.polder.Step()
	_, sts = r.replies(t)
	if len(sts) != 2 || sts[0].Meaning != edi.PickedUp || sts[1].Meaning != edi.Departed || sts[0].UNLocode != "NLTLB" ||
		sts[0].TransportDocument != "ECMR-NL-2026-004471" || !sts[0].Time.Equal(time.Date(2026, 10, 21, 7, 0, 0, 0, time.UTC)) {
		t.Fatalf("statuses %+v", sts)
	}
	r.now = r.now.Add(10 * time.Second)
	_ = r.polder.Step()
	_, sts = r.replies(t)
	if len(sts) != 2 || sts[0].Meaning != edi.GateIn || sts[1].Meaning != edi.Delivered || sts[1].UNLocode != "NLRTM" ||
		!sts[0].Time.Equal(time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("statuses %+v", sts)
	}
	if len(r.polder.trips) != 0 {
		t.Error("a finished trip is kept")
	}
}

// TestEDIFACTResendAndRefuse: an instruction sent again gets the same
// acknowledgement and no second trip; one from another partner, or
// malformed, isn't taken.
func TestEDIFACTResendAndRefuse(t *testing.T) {
	r := newRig(t)
	for range 2 {
		if _, err := r.toPol.Put("MF000031", instruction("meridian-forwarding.example")); err != nil {
			t.Fatal(err)
		}
		_ = r.polder.Step()
	}
	acks, _ := r.replies(t)
	if len(acks) != 2 || acks[0].CarrierRef != acks[1].CarrierRef || len(r.polder.trips) != 1 {
		t.Errorf("acks %+v, %d trips", acks, len(r.polder.trips))
	}
	_, _ = r.toPol.Put("MF000032", instruction("mallory.example"))
	_, _ = r.toPol.Put("MF000033", []byte("UNH+1+IFTMIN:D:04A:UN'UNT+2+1'"))
	_ = r.polder.Step()
	if acks, _ := r.replies(t); len(acks) != 0 || len(r.polder.trips) != 1 {
		t.Errorf("took a stranger's or a malformed instruction: %+v", acks)
	}
}

// TestEDIFACTRun: Run steps until its context ends.
func TestEDIFACTRun(t *testing.T) {
	r := newRig(t)
	r.polder.Now = time.Now
	if _, err := r.toPol.Put("MF000031", instruction("meridian-forwarding.example")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := r.polder.Run(ctx, 20*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if acks, _ := r.replies(t); len(acks) != 1 {
		t.Errorf("acks %+v", acks)
	}
}
