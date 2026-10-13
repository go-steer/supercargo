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

package forwarder

import (
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/edi"
)

// TestHeldStatuses: a status taken before its leg is recorded is held and
// handed on once the leg is there; one whose leg never comes is dropped
// after HoldFor.
func TestHeldStatuses(t *testing.T) {
	r := newEDIRig(t)
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	r.gw.Now = func() time.Time { return now }
	recorded := false
	var handed []edi.Status
	r.gw.Status = func(_ string, s edi.Status) error {
		if !recorded {
			return ErrNoLeg
		}
		handed = append(handed, s)
		return nil
	}
	r.gw.instructed = map[string]instructed{"MF-0007-L1": {carrier: polder, ack: &edi.Ack{Accepted: true}}}
	data, err := edi.WriteIFTSTA([]edi.Status{{Reference: "MF-0007-L1", Code: "12", Time: now}},
		edi.Envelope{Sender: polder, Recipient: meridian, Control: "PH1", At: now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.gw.Partners[polder].In.Put("PH1", data); err != nil {
		t.Fatal(err)
	}
	r.gw.Step()
	if len(r.gw.held) != 1 || len(handed) != 0 {
		t.Fatalf("held %d, handed %d", len(r.gw.held), len(handed))
	}
	recorded = true
	r.gw.Step()
	if len(r.gw.held) != 0 || len(handed) != 1 {
		t.Fatalf("after the leg: held %d, handed %d", len(r.gw.held), len(handed))
	}
	// A leg that never comes: dropped after HoldFor.
	recorded = false
	r.gw.held = []heldStatus{{carrier: polder, status: edi.Status{Reference: "MF-0009-L1"}, since: now}}
	now = now.Add(HoldFor + time.Second)
	r.gw.Step()
	if len(r.gw.held) != 0 {
		t.Errorf("kept past HoldFor: %d", len(r.gw.held))
	}
}
