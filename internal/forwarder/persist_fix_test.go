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
	"context"
	"testing"
	"time"
)

// TestGatewayUnwritten: an instruction or cancellation saved but never
// written (the node stopped between the two) is written when asked
// again, in the interchange it was saved with, and its answer taken.
func TestGatewayUnwritten(t *testing.T) {
	ctx := context.Background()
	d := openDB(t)
	r := newEDIRig(t)
	r.polder.HoursPerSecond = 0.01
	r.run(t)
	i := anInstruction()
	_ = d.Bucket("edi").Put(i.Reference, savedInstruction{Carrier: polder, Instruction: i, Control: "G0000000000077"})
	if err := r.gw.Persist(d.Bucket("edi")); err != nil {
		t.Fatal(err)
	}
	took, err := r.gw.Instruct(ctx, polder, i)
	if err != nil || !took.Accepted || took.Answers != "G0000000000077" {
		t.Fatalf("an unwritten instruction: %+v, %v", took, err)
	}
	r.gw.mu.Lock()
	in := r.gw.instructed[i.Reference]
	in.cancel = &cancelling{control: "G0000000000078"}
	r.gw.instructed[i.Reference] = in
	r.gw.mu.Unlock()
	if ack, err := r.gw.Cancel(ctx, polder, i.Reference); err != nil || !ack.Accepted || ack.Answers != "G0000000000078" {
		t.Errorf("an unwritten cancellation: %+v, %v", ack, err)
	}
	again := &Gateway{Self: meridian, Partners: r.gw.Partners, Now: time.Now, Log: quiet}
	_ = again.Persist(d.Bucket("edi"))
	if in := again.instructed[i.Reference]; !in.written || in.cancel == nil || !in.cancel.written {
		t.Errorf("written not saved: %+v", in)
	}
}

// TestConfirmSaveFails: a proposal whose sending can't be saved isn't
// sent, and stays open.
func TestConfirmSaveFails(t *testing.T) {
	r, _, am := relayed(t)
	_ = r.Persist(RelaySavers{failingSaver{}, failingSaver{}, failingSaver{}, failingSaver{}})
	if err := r.Event(shipLeg, event("EV-1", "est")); err != nil {
		t.Fatal(err)
	}
	if err := r.Alert(shipLeg, delay("AL-1", "EV-1", "2026-10-23T14:00:00Z", "2026-10-26T14:00:00Z")); err != nil {
		t.Fatal(err)
	}
	id := r.Proposals("MF-0007")[0].ID
	if p, err := r.Confirm(context.Background(), id); err == nil || p.Status != ProposalOpen || am.got != nil {
		t.Errorf("sent without saving: %+v, %v, amendment %+v", p, err, am.got)
	}
}

// TestRelayedDuplicates: a leg record saved twice (an eviction whose
// delete failed, then remembered again) loads once, the later entry.
func TestRelayedDuplicates(t *testing.T) {
	d := openDB(t)
	b := d.Bucket("r")
	_ = b.Put(relayedKey(1), savedRelayed{"ocean-blue.example", "OB-BKG-1042", "EV-1", "MF-EV-000001"})
	_ = b.Put(relayedKey(5), savedRelayed{"ocean-blue.example", "OB-BKG-1042", "EV-1", "MF-EV-000009"})
	r, _, _ := relayed(t)
	if err := r.Persist(relaySavers(d)); err != nil {
		t.Fatal(err)
	}
	if len(r.order) != 1 || r.order[0].n != 5 || r.relayed[legEvent{"ocean-blue.example", "OB-BKG-1042", "EV-1"}] != "MF-EV-000009" || r.relayedN != 5 {
		t.Errorf("order %+v, relayed %+v", r.order, r.relayed)
	}
	// Forgetting before the oldest kept drops the older keys.
	r.mu.Lock()
	r.remember(legEvent{"ocean-blue.example", "OB-BKG-1042", "EV-2"}, "MF-EV-000010")
	r.forgetRelayedBefore(r.order[1])
	r.mu.Unlock()
	var keys []string
	_ = b.Load(func(k string, _ []byte) error { keys = append(keys, k); return nil })
	if len(keys) != 1 || keys[0] != relayedKey(6) {
		t.Errorf("kept %v", keys)
	}
}
