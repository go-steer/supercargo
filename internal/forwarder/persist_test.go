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
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/edi"
	"github.com/go-steer/supercargo/internal/persist"
)

func openDB(t *testing.T) *persist.DB {
	t.Helper()
	d, err := persist.Open(filepath.Join(t.TempDir(), "mf.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func relaySavers(d *persist.DB) RelaySavers {
	return RelaySavers{Statuses: d.Bucket("s"), Legs: d.Bucket("l"), Relayed: d.Bucket("r"), Proposals: d.Bucket("p")}
}

// TestRelayPersist: after a restart the relay still knows each re-issued
// record's leg, which leg record each came from (so a leg's alert cites
// the order's events), the EDI statuses re-issued and the proposals; new
// proposals number on after them, and one that was being sent is closed.
func TestRelayPersist(t *testing.T) {
	d := openDB(t)
	r, _, am := relayed(t)
	if err := r.Persist(relaySavers(d)); err != nil {
		t.Fatal(err)
	}
	_ = r.Event(shipLeg, event("EV-1", "est"))
	_ = r.Alert(shipLeg, delay("AL-1", "EV-1", "2026-10-23T14:00:00Z", "2026-10-26T14:00:00Z"))
	ps := r.Proposals("MF-0007")
	if len(ps) != 1 {
		t.Fatalf("proposals %+v", ps)
	}
	r.mu.Lock()
	r.statuses[statusKey{"polder", "MF-0007-L1", "12", "2026-10-21T07:00:00Z"}] = true
	r.saveStatus(statusKey{"polder", "MF-0007-L1", "12", "2026-10-21T07:00:00Z"})
	r.proposals[ps[0].ID].Status = ProposalSending
	r.saveProposal(r.proposals[ps[0].ID])
	r.mu.Unlock()

	again := &Relay{Orders: r.Orders, Publish: &published{}, Amend: am, Now: time.Now}
	if err := again.Persist(relaySavers(d)); err != nil {
		t.Fatal(err)
	}
	if leg, ok := again.LegOf("MF-EV-000001"); !ok || leg != 1 {
		t.Errorf("leg of the re-issued event: %d, %v", leg, ok)
	}
	if got := again.relayed[legEvent{"ocean-blue.example", "OB-BKG-1042", "EV-1"}]; got != "MF-EV-000001" {
		t.Errorf("the leg event's order event: %q", got)
	}
	if !again.statuses[statusKey{"polder", "MF-0007-L1", "12", "2026-10-21T07:00:00Z"}] {
		t.Error("a re-issued status forgotten")
	}
	restored := again.Proposals("MF-0007")
	if len(restored) != 1 || restored[0].Status != ProposalFailed || !reflect.DeepEqual(restored[0].Amendment, ps[0].Amendment) {
		t.Errorf("proposals %+v", restored)
	}
	if id := again.nextID("PR"); id <= ps[0].ID {
		t.Errorf("a new proposal's ID %s isn't after %s", id, ps[0].ID)
	}
	// The statuses and legs start over in a new generation: the old ones go.
	again.mu.Lock()
	again.statusesOver()
	again.legsOver()
	again.mu.Unlock()
	third := &Relay{Orders: r.Orders, Publish: &published{}, Amend: am, Now: time.Now}
	_ = third.Persist(relaySavers(d))
	if len(third.statuses) != 0 || len(third.legs) != 0 {
		t.Errorf("after starting over: %d statuses, %d legs", len(third.statuses), len(third.legs))
	}
}

// failingSaver refuses every write and reports it.
type failingSaver struct{ failing }

// TestRelaySaveErrors: a change the file refuses is reported, not fatal.
func TestRelaySaveErrors(t *testing.T) {
	r, _, _ := relayed(t)
	var errs []error
	r.OnSaveError = func(err error) { errs = append(errs, err) }
	if err := r.Persist(RelaySavers{failingSaver{}, failingSaver{}, failingSaver{}, failingSaver{}}); err != nil {
		t.Fatal(err)
	}
	if err := r.Event(shipLeg, event("EV-1", "est")); err != nil {
		t.Fatal(err)
	}
	if len(errs) == 0 {
		t.Error("no save error reported")
	}
	bad := openDB(t)
	_ = bad.Bucket("p").Put("x", "not a proposal")
	if err := r.Persist(RelaySavers{bad.Bucket("s"), bad.Bucket("l"), bad.Bucket("r"), bad.Bucket("p")}); err == nil {
		t.Error("loaded a proposal that isn't one")
	}
}

// TestGatewayPersist: a restarted gateway knows the instructions sent and
// their answers, takes answers to them, and can cancel them; an
// instruction the file refuses isn't sent.
func TestGatewayPersist(t *testing.T) {
	ctx := context.Background()
	d := openDB(t)
	r := newEDIRig(t)
	r.polder.HoursPerSecond = 0.01
	if err := r.gw.Persist(d.Bucket("edi")); err != nil {
		t.Fatal(err)
	}
	r.run(t)
	took, err := r.gw.Instruct(ctx, polder, anInstruction())
	if err != nil || !took.Accepted {
		t.Fatalf("instruct: %+v, %v", took, err)
	}
	again := &Gateway{Self: meridian, Partners: r.gw.Partners, AckTimeout: 5 * time.Second, PollEvery: 20 * time.Millisecond, Now: time.Now, Log: quiet}
	if err := again.Persist(d.Bucket("edi")); err != nil {
		t.Fatal(err)
	}
	if _, ack, ok := again.Instructed("MF-0007-L1"); !ok || ack == nil || ack.CarrierRef != took.CarrierRef {
		t.Fatalf("after a restart: %+v, %v", ack, ok)
	}
	if ack, err := again.Cancel(ctx, polder, "MF-0007-L1"); err != nil || !ack.Accepted {
		t.Fatalf("cancelled after a restart: %+v, %v", ack, err)
	}
	third := &Gateway{Self: meridian, Partners: r.gw.Partners, Now: time.Now, Log: quiet}
	_ = third.Persist(d.Bucket("edi"))
	if !third.Withdrawn("MF-0007-L1") {
		t.Error("the cancellation's answer forgotten")
	}
	// An instruction the file refuses isn't written.
	refused := &Gateway{Self: meridian, Partners: r.gw.Partners, Now: time.Now, Log: quiet}
	_ = refused.Persist(failingSaver{})
	i := anInstruction()
	i.Reference = "MF-0008-L1"
	if _, err := refused.Instruct(ctx, polder, i); err == nil {
		t.Error("instructed without saving")
	}
	if _, _, ok := refused.Instructed("MF-0008-L1"); ok {
		t.Error("an unsaved instruction kept")
	}
	refused.instructed["MF-0007-L1"] = instructed{carrier: polder, control: "G1", ack: &edi.Ack{Reference: "MF-0007-L1", CarrierRef: "PH1", Accepted: true}}
	if _, err := refused.Cancel(ctx, polder, "MF-0007-L1"); err == nil {
		t.Error("cancelled without saving")
	}
	bad := openDB(t)
	_ = bad.Bucket("edi").Put("x", "not an instruction")
	if err := third.Persist(bad.Bucket("edi")); err == nil {
		t.Error("loaded an instruction that isn't one")
	}
}
