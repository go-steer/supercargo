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
	"errors"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/edi"
	"github.com/go-steer/supercargo/internal/mockcarrier"
)

// TestGatewayCancel: a leg the carrier took is cancelled; cancelling
// again sends nothing new; its statuses are refused from then on.
func TestGatewayCancel(t *testing.T) {
	ctx := context.Background()
	r := newEDIRig(t)
	r.polder.HoursPerSecond = 0.01 // nothing moves for minutes
	r.run(t)
	took, err := r.gw.Instruct(ctx, polder, anInstruction())
	if err != nil || !took.Accepted {
		t.Fatalf("instruct: %+v, %v", took, err)
	}
	ack, err := r.gw.Cancel(ctx, polder, "MF-0007-L1")
	if err != nil || !ack.Accepted || ack.CarrierRef != took.CarrierRef || ack.Answers == took.Answers {
		t.Fatalf("cancel: %+v, %v", ack, err)
	}
	if again, err := r.gw.Cancel(ctx, polder, "MF-0007-L1"); err != nil || again.Answers != ack.Answers {
		t.Errorf("again: %+v, %v", again, err)
	}
	// A status for the cancelled leg isn't handed on.
	st, err := edi.WriteIFTSTA([]edi.Status{{Reference: "MF-0007-L1", CarrierRef: took.CarrierRef, Code: "12", Time: time.Now().UTC().Truncate(time.Minute), UNLocode: "NLTLB"}},
		edi.Envelope{Sender: polder, Recipient: meridian, Control: "PH999999", At: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.polder.Out.Put("PH999999", st); err != nil {
		t.Fatal(err)
	}
	r.gw.Step()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.got) != 0 {
		t.Errorf("a cancelled leg's status handed on: %+v", r.got)
	}
}

// TestGatewayCancelRefused: once the truck picked up, the carrier refuses;
// what was never taken, or isn't a partner's, can't be cancelled; a
// cancellation isn't an instruction.
func TestGatewayCancelRefused(t *testing.T) {
	ctx := context.Background()
	r := newEDIRig(t)
	r.run(t)
	if _, err := r.gw.Cancel(ctx, polder, "MF-0007-L1"); !errors.Is(err, ErrNotTaken) {
		t.Errorf("never instructed: %v", err)
	}
	if _, err := r.gw.Cancel(ctx, "nobody.example", "MF-0007-L1"); !errors.Is(err, ErrNoPartner) {
		t.Errorf("not a partner: %v", err)
	}
	if _, err := r.gw.Instruct(ctx, polder, edi.Instruction{Reference: "MF-0007-L1", Cancel: true}); err == nil {
		t.Error("a cancellation instructed")
	}
	if _, err := r.gw.Instruct(ctx, polder, anInstruction()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		r.gw.Step()
		r.mu.Lock()
		n := len(r.got)
		r.mu.Unlock()
		if n > 0 {
			break // picked up
		}
		if time.Now().After(deadline) {
			t.Fatal("no status")
		}
		time.Sleep(20 * time.Millisecond)
	}
	ack, err := r.gw.Cancel(ctx, polder, "MF-0007-L1")
	if err != nil || ack.Accepted || len(ack.Errors) != 1 || ack.Errors[0] != mockcarrier.UnderWay {
		t.Errorf("under way: %+v, %v", ack, err)
	}
}

// TestGatewayCancelThenBook: a cancelled instruction isn't instructed
// again; booking the leg again goes under the next reference.
func TestGatewayCancelThenBook(t *testing.T) {
	ctx := context.Background()
	r := newEDIRig(t)
	r.polder.HoursPerSecond = 0.01
	r.run(t)
	if _, err := r.gw.Instruct(ctx, polder, anInstruction()); err != nil {
		t.Fatal(err)
	}
	if r.gw.Withdrawn("MF-0007-L1") {
		t.Error("withdrawn before it was cancelled")
	}
	if ack, err := r.gw.Cancel(ctx, polder, "MF-0007-L1"); err != nil || !ack.Accepted {
		t.Fatalf("cancel: %+v, %v", ack, err)
	}
	if !r.gw.Withdrawn("MF-0007-L1") {
		t.Error("not withdrawn")
	}
	if _, err := r.gw.Instruct(ctx, polder, anInstruction()); !errors.Is(err, ErrCancelledLeg) {
		t.Errorf("instructed again: %v", err)
	}
}

// TestGatewayCancelEdges: only the carrier instructed, and only an
// instruction it took, can be cancelled; a cancellation that can't be
// written is forgotten, so it can be tried again; an answer from another
// sender isn't taken.
func TestGatewayCancelEdges(t *testing.T) {
	ctx := context.Background()
	r := newEDIRig(t)
	other := *r.gw.Partners[polder]
	other.ID = "other.example"
	r.gw.Partners["other.example"] = &other
	r.gw.instructed = map[string]instructed{
		"MF-0007-L1": {carrier: polder, control: "G1", ack: &edi.Ack{Reference: "MF-0007-L1", CarrierRef: "PH4471", Accepted: true}},
		"MF-0007-L2": {carrier: polder, control: "G2", ack: &edi.Ack{Reference: "MF-0007-L2", CarrierRef: "PH4472", Errors: []string{"X"}}},
	}
	if _, err := r.gw.Cancel(ctx, "other.example", "MF-0007-L1"); !errors.Is(err, ErrNotTaken) {
		t.Errorf("another partner: %v", err)
	}
	if _, err := r.gw.Cancel(ctx, polder, "MF-0007-L2"); !errors.Is(err, ErrNotTaken) {
		t.Errorf("refused by the carrier: %v", err)
	}
	from := func(sender string) []byte {
		data, err := edi.WriteAPERAK(edi.Ack{Reference: "MF-0007-L1", CarrierRef: "PH4471", Accepted: true, Answers: "G1"},
			edi.Envelope{Sender: sender, Recipient: meridian, Control: "X1", At: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	if err := r.gw.ack(polder, r.gw.Partners[polder], from("mallory.example")); err == nil {
		t.Error("an answer from another sender")
	}
	if err := r.gw.ack("other.example", &other, from("other.example")); err == nil {
		t.Error("an answer for another carrier's instruction")
	}
	_ = r.gw.Partners[polder].Out.Close()
	if _, err := r.gw.Cancel(ctx, polder, "MF-0007-L1"); err == nil {
		t.Fatal("a cancellation that couldn't be written")
	}
	if in := r.gw.instructed["MF-0007-L1"]; in.cancel != nil {
		t.Error("a cancellation never written is still awaited")
	}
}

// TestGatewayHeldCancelled: a status held for a leg not yet recorded is
// dropped once the leg's cancellation is accepted.
func TestGatewayHeldCancelled(t *testing.T) {
	r := newEDIRig(t)
	handed := 0
	r.gw.Status = func(string, edi.Status) error { handed++; return nil }
	accepted := &edi.Ack{Reference: "MF-0007-L1", CarrierRef: "PH4471", Accepted: true}
	r.gw.instructed = map[string]instructed{"MF-0007-L1": {carrier: polder, control: "G1", ack: accepted,
		cancel: &cancelling{control: "G2", ack: accepted}}}
	r.gw.held = []heldStatus{{carrier: polder, status: edi.Status{Reference: "MF-0007-L1"}, since: time.Now()}}
	r.gw.retryHeld()
	if handed != 0 || len(r.gw.held) != 0 {
		t.Errorf("handed %d, held %d", handed, len(r.gw.held))
	}
}

// TestGatewayAckMatching: an acknowledgement answers the interchange it
// names; one naming an interchange we never sent is refused, and one
// naming none is the instruction's, never its cancellation's.
func TestGatewayAckMatching(t *testing.T) {
	r := newEDIRig(t)
	p := r.gw.Partners[polder]
	ack := func(answers string, accepted bool) []byte {
		a := edi.Ack{Reference: "MF-0007-L1", CarrierRef: "PH4471", Accepted: accepted, Answers: answers}
		if !accepted {
			a.Errors = []string{"UWY"}
		}
		data, err := edi.WriteAPERAK(a, edi.Envelope{Sender: polder, Recipient: meridian, Control: "PH000001", At: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	r.gw.instructed = map[string]instructed{"MF-0007-L1": {carrier: polder, control: "G1", cancel: &cancelling{control: "G2"},
		ack: &edi.Ack{Reference: "MF-0007-L1", CarrierRef: "PH4471", Accepted: true}}}
	if err := r.gw.ack(polder, p, ack("G9", true)); err == nil {
		t.Error("an answer to an interchange we never sent")
	}
	if err := r.gw.ack(polder, p, ack("", false)); err == nil {
		t.Error("an answer naming none, while the cancellation awaits its answer, taken")
	}
	if in := r.gw.instructed["MF-0007-L1"]; in.cancel.ack != nil || !in.ack.Accepted {
		t.Errorf("an answer naming none taken as the cancellation's: %+v", in)
	}
	if err := r.gw.ack(polder, p, ack("G2", false)); err != nil {
		t.Fatal(err)
	}
	if err := r.gw.ack(polder, p, ack("G2", true)); err != nil {
		t.Fatal(err)
	}
	if in := r.gw.instructed["MF-0007-L1"]; in.cancel.ack == nil || in.cancel.ack.Accepted || in.cancelled() {
		t.Errorf("the cancellation's first answer: %+v", in.cancel.ack)
	}
}
