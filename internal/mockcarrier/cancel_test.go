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
	"strings"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/edi"
)

func cancellation(control string) []byte {
	data, err := edi.WriteIFTMIN(edi.Instruction{Reference: "MF-0007-L1", Cancel: true},
		edi.Envelope{Sender: "meridian-forwarding.example", Recipient: "polder-haulage.example", Control: control, At: time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)})
	if err != nil {
		panic(err)
	}
	return data
}

func (r *rig) put(t *testing.T, control string, data []byte) {
	t.Helper()
	if _, err := r.toPol.Put(control, data); err != nil {
		t.Fatal(err)
	}
	if err := r.polder.Step(); err != nil {
		t.Fatal(err)
	}
}

// TestEDIFACTCancel: before the truck picks up, a cancellation is
// accepted, naming the interchange it answers, and the trip ends; asked
// again, the same answer.
func TestEDIFACTCancel(t *testing.T) {
	r := newRig(t)
	r.put(t, "MF000031", instruction("meridian-forwarding.example"))
	acks, _ := r.replies(t)
	if len(acks) != 1 || acks[0].Answers != "MF000031" {
		t.Fatalf("the instruction's acknowledgement: %+v", acks)
	}
	r.put(t, "MF000032", cancellation("MF000032"))
	acks, _ = r.replies(t)
	if len(acks) != 1 || !acks[0].Accepted || acks[0].Answers != "MF000032" || acks[0].CarrierRef != "PH4471" {
		t.Fatalf("cancelled: %+v", acks)
	}
	r.now = r.now.Add(time.Minute) // the whole trip, at 1 h/s
	if err := r.polder.Step(); err != nil {
		t.Fatal(err)
	}
	if _, sts := r.replies(t); len(sts) != 0 {
		t.Errorf("a cancelled trip reported %d statuses", len(sts))
	}
	r.put(t, "MF000033", cancellation("MF000033"))
	if acks, _ := r.replies(t); len(acks) != 1 || !acks[0].Accepted || acks[0].Answers != "MF000033" {
		t.Errorf("again: %+v", acks)
	}
}

// TestEDIFACTReack: an instruction sent again in another interchange (a
// forwarder restarted) is answered again, naming that interchange; a
// cancellation from anyone but the partner isn't taken.
func TestEDIFACTReack(t *testing.T) {
	r := newRig(t)
	r.put(t, "MF000031", instruction("meridian-forwarding.example"))
	first, _ := r.replies(t)
	data := []byte(strings.ReplaceAll(string(instruction("meridian-forwarding.example")), "MF000031", "MF000041"))
	r.put(t, "MF000041", data)
	acks, _ := r.replies(t)
	if len(acks) != 1 || acks[0].Answers != "MF000041" || acks[0].CarrierRef != first[0].CarrierRef || !acks[0].Accepted {
		t.Errorf("again: %+v", acks)
	}
	stranger, err := edi.WriteIFTMIN(edi.Instruction{Reference: "MF-0007-L1", Cancel: true},
		edi.Envelope{Sender: "mallory.example", Recipient: "polder-haulage.example", Control: "M1", At: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	r.put(t, "M1", stranger)
	if acks, _ := r.replies(t); len(acks) != 0 {
		t.Errorf("a stranger's cancellation answered: %+v", acks)
	}
	if r.polder.cancelled["MF-0007-L1"] {
		t.Error("a stranger cancelled the trip")
	}
}

// TestEDIFACTCancelRefused: once the truck has picked up, a cancellation
// is refused (under way) and the trip goes on; one of an instruction
// never taken isn't answered.
func TestEDIFACTCancelRefused(t *testing.T) {
	r := newRig(t)
	r.put(t, "MF000031", instruction("meridian-forwarding.example"))
	r.now = r.now.Add(2 * time.Second) // picked up, departed
	if err := r.polder.Step(); err != nil {
		t.Fatal(err)
	}
	_, _ = r.replies(t)
	r.put(t, "MF000032", cancellation("MF000032"))
	acks, _ := r.replies(t)
	if len(acks) != 1 || acks[0].Accepted || len(acks[0].Errors) != 1 || acks[0].Errors[0] != UnderWay {
		t.Fatalf("under way: %+v", acks)
	}
	r.now = r.now.Add(time.Minute)
	if err := r.polder.Step(); err != nil {
		t.Fatal(err)
	}
	if _, sts := r.replies(t); len(sts) == 0 {
		t.Error("the trip stopped")
	}
	fresh := newRig(t)
	fresh.put(t, "MF000032", cancellation("MF000032"))
	if acks, _ := fresh.replies(t); len(acks) != 0 {
		t.Errorf("a cancellation of nothing answered: %+v", acks)
	}
}
