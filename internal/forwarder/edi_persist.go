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

	"github.com/go-steer/supercargo/internal/edi"
	"github.com/go-steer/supercargo/internal/store"
)

// savedInstruction is an instruction sent, as saved.
type savedInstruction struct {
	Carrier     string
	Instruction edi.Instruction
	Control     string
	Written     bool
	Ack         *edi.Ack
	Cancel      *savedCancel
}

type savedCancel struct {
	Control string
	Written bool
	Ack     *edi.Ack
}

// Persist loads the instructions sv holds into g, then saves each one
// sent, answered or cancelled (docs/DESIGN.md, A.12 "persistence"). An
// instruction or cancellation is saved before its file is written, and
// not written if the save fails, so a restarted gateway knows of every
// one it sent and takes their answers. An answer that can't be saved is
// kept in memory and logged. Call it once, before g is used.
func (g *Gateway) Persist(sv store.Saver) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.instructed = map[string]instructed{}
	if err := sv.Load(func(key string, raw []byte) error {
		var s savedInstruction
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
		in := instructed{carrier: s.Carrier, instruction: s.Instruction, control: s.Control, written: s.Written, ack: s.Ack}
		if s.Cancel != nil {
			in.cancel = &cancelling{control: s.Cancel.Control, written: s.Cancel.Written, ack: s.Cancel.Ack}
		}
		g.instructed[key] = in
		return nil
	}); err != nil {
		return err
	}
	g.sv = sv
	return nil
}

// saveLocked saves the instruction under ref as it stands. g.mu is held.
func (g *Gateway) saveLocked(ref string) error {
	if g.sv == nil {
		return nil
	}
	in, ok := g.instructed[ref]
	if !ok {
		return g.sv.Delete(ref)
	}
	s := savedInstruction{Carrier: in.carrier, Instruction: in.instruction, Control: in.control, Written: in.written, Ack: in.ack}
	if in.cancel != nil {
		s.Cancel = &savedCancel{Control: in.cancel.control, Written: in.cancel.written, Ack: in.cancel.ack}
	}
	return g.sv.Put(ref, s)
}

// keepLocked saves a change after the fact (a file written, an answer
// taken, a record dropped): logged if the file refuses it, since what it
// records has happened. g.mu is held.
func (g *Gateway) keepLocked(ref string) {
	if err := g.saveLocked(ref); err != nil {
		g.Log.Error("edi state not saved", "reference", ref, "error", err)
	}
}
