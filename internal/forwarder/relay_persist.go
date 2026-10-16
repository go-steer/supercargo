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
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/go-steer/supercargo/internal/store"
)

// RelaySavers keep what the relay knows across restarts
// (docs/DESIGN.md, A.12 "persistence"), each in its own bucket: the EDI
// statuses re-issued, each re-issued record's leg, which leg record each
// order record was re-issued from, and the proposals.
type RelaySavers struct {
	Statuses, Legs, Relayed, Proposals store.Saver
}

type savedStatus struct{ Carrier, Ref, Code, At string }

type savedRelayed struct {
	Carrier, Ref, ID string // the leg's record
	Order            string // the order's record re-issued from it
}

// Persist loads what s holds into r, then saves each change. Saving is
// best effort: what the relay keeps saves duplicates and lets alerts cite
// across a restart, and a failed save (reported to OnSaveError) is no
// reason to refuse a leg's event that was already re-issued. A proposal
// that was being sent when the node stopped is closed as failed: whether
// its carrier took the amendment is for a person to check. Call it once,
// before r is used.
func (r *Relay) Persist(s RelaySavers) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statuses, r.legs, r.relayed, r.proposals = map[statusKey]bool{}, map[string]int{}, map[legEvent]string{}, map[string]*Proposal{}
	if err := s.Statuses.Load(func(key string, raw []byte) error {
		var v savedStatus
		if err := json.Unmarshal(raw, &v); err != nil {
			return err
		}
		r.statuses[statusKey{v.Carrier, v.Ref, v.Code, v.At}] = true
		r.statusGen = max(r.statusGen, generation(key))
		return nil
	}); err != nil {
		return err
	}
	if err := s.Legs.Load(func(key string, raw []byte) error {
		var leg int
		if err := json.Unmarshal(raw, &leg); err != nil {
			return err
		}
		_, id, _ := strings.Cut(key, " ")
		r.legs[id] = leg
		r.legGen = max(r.legGen, generation(key))
		return nil
	}); err != nil {
		return err
	}
	if err := s.Relayed.Load(func(key string, raw []byte) error {
		var v savedRelayed
		if err := json.Unmarshal(raw, &v); err != nil {
			return err
		}
		n, err := strconv.ParseUint(key, 10, 64)
		if err != nil {
			return err
		}
		k := legEvent{v.Carrier, v.Ref, v.ID}
		if _, dup := r.relayed[k]; dup {
			// Remembered again after an eviction whose delete failed: the
			// later entry stands.
			r.order = slices.DeleteFunc(r.order, func(e relayedEntry) bool { return e.k == k })
		}
		r.relayed[k] = v.Order
		r.order = append(r.order, relayedEntry{k, n})
		r.relayedN = max(r.relayedN, n)
		return nil
	}); err != nil {
		return err
	}
	var interrupted []*Proposal
	if err := s.Proposals.Load(func(_ string, raw []byte) error {
		var p Proposal
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		if p.Status == ProposalSending {
			p.Status, p.Outcome = ProposalFailed, "the node stopped while it was being sent (the carrier may hold the new plan: check with them)"
			interrupted = append(interrupted, &p)
		}
		r.proposals[p.ID] = &p
		return nil
	}); err != nil {
		return err
	}
	ids := make([]string, 0, len(r.proposals))
	for id := range r.proposals {
		ids = append(ids, id)
	}
	r.resumeLocked(ids) // a new proposal's ID after those kept
	r.save = &s
	for _, p := range interrupted {
		r.saveProposal(p)
	}
	return nil
}

// relayedEntry is a remembered leg record, in the order remembered.
type relayedEntry struct {
	k legEvent
	n uint64
}

// generation is the generation a statuses or legs key was saved in: the
// map starts over past MaxRelayed, in a new one.
func generation(key string) int {
	g, _, _ := strings.Cut(key, " ")
	n, _ := strconv.Atoi(g)
	return n
}

func genKey(gen int, rest string) string { return fmt.Sprintf("%06d %s", gen, rest) }

// saved reports a save that failed. r.mu is held.
func (r *Relay) saved(err error) {
	if err != nil && r.OnSaveError != nil {
		r.OnSaveError(err)
	}
}

func (r *Relay) saveStatus(k statusKey) {
	if r.save != nil {
		r.saved(r.save.Statuses.Put(genKey(r.statusGen, k.carrier+" "+k.ref+" "+k.code+" "+k.at), savedStatus{k.carrier, k.ref, k.code, k.at}))
	}
}

// statusesOver starts the statuses over, in a new generation. r.mu is held.
func (r *Relay) statusesOver() {
	r.statuses = map[statusKey]bool{}
	r.statusGen++
	if r.save != nil {
		r.saved(r.save.Statuses.DeleteBefore(genKey(r.statusGen, "")))
	}
}

func (r *Relay) saveLeg(id string, leg int) {
	if r.save != nil {
		r.saved(r.save.Legs.Put(genKey(r.legGen, id), leg))
	}
}

// legsOver starts the legs over, in a new generation. r.mu is held.
func (r *Relay) legsOver() {
	r.legs = map[string]int{}
	r.legGen++
	if r.save != nil {
		r.saved(r.save.Legs.DeleteBefore(genKey(r.legGen, "")))
	}
}

func relayedKey(n uint64) string { return fmt.Sprintf("%020d", n) }

func (r *Relay) saveRelayed(e relayedEntry, order string) {
	if r.save != nil {
		r.saved(r.save.Relayed.Put(relayedKey(e.n), savedRelayed{e.k.carrier, e.k.ref, e.k.id, order}))
	}
}

// forgetRelayedBefore drops the remembered leg records older than the
// oldest kept. r.mu is held.
func (r *Relay) forgetRelayedBefore(oldest relayedEntry) {
	if r.save != nil {
		r.saved(r.save.Relayed.DeleteBefore(relayedKey(oldest.n)))
	}
}

func (r *Relay) saveProposal(p *Proposal) {
	if r.save != nil {
		r.saved(r.save.Proposals.Put(p.ID, p))
	}
}

func (r *Relay) forgetProposal(id string) {
	if r.save != nil {
		r.saved(r.save.Proposals.Delete(id))
	}
}
