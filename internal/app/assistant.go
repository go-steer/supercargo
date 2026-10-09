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

package app

import (
	"errors"
	"fmt"
	"time"

	"github.com/go-steer/supercargo/internal/assistant"
	"github.com/go-steer/supercargo/internal/business"
	"github.com/go-steer/supercargo/internal/config"
	"github.com/go-steer/supercargo/internal/directory"
	"github.com/go-steer/supercargo/internal/skills"
)

// ShipperAssistant builds the shipper role's assistant (docs/DESIGN.md,
// A.10, LLM #1) for cfg, reaching its node through node: the socket for
// ulpchat, in process for the browser UI. Each caller gets its own, with
// its own drafts: one assistant per role, one conversation each (#75).
func ShipperAssistant(cfg *config.Config, node assistant.Node) (*assistant.Assistant, error) {
	if !cfg.Has(config.RoleShipper) {
		return nil, fmt.Errorf("%s has roles %v; the assistant is the shipper's so far, and needs the shipper role (#75)", cfg.Domain, cfg.Roles)
	}
	if cfg.LLM == nil {
		return nil, errors.New("the configuration has no llm section: the assistant needs a model (provider: fake needs no key)")
	}
	model, err := NewModel(cfg.LLM, assistant.FakeModel(time.Now))
	if err != nil {
		return nil, err
	}
	agentSkills := assistant.BuiltinSkills()
	if cfg.Skills != nil {
		company, err := skills.LoadDir(cfg.Skills.Dir)
		if err != nil {
			return nil, err
		}
		agentSkills = append(agentSkills, company...)
	}
	a, err := ToolAssistant(cfg, node)
	if err != nil {
		return nil, err
	}
	a.Model, a.Skills = model, agentSkills
	return a, nil
}

// ToolAssistant is the shipper's assistant without a model: its tools,
// their checks, its partners and its drafts, for an outside agent to call
// over MCP (internal/mcpserver). Its business dates and notes are private,
// as for the assistant: no question to a carrier may carry them.
func ToolAssistant(cfg *config.Config, node assistant.Node) (*assistant.Assistant, error) {
	if !cfg.Has(config.RoleShipper) {
		return nil, fmt.Errorf("%s has roles %v; the shipper's tools need the shipper role", cfg.Domain, cfg.Roles)
	}
	book, dir, err := Partners(cfg)
	if err != nil {
		return nil, err
	}
	a := &assistant.Assistant{Node: node, Drafts: &assistant.Drafts{},
		Self: assistant.Self{Domain: cfg.Domain, Name: cfg.Name, Endpoint: RESTURL(cfg)}, AddressBook: book}
	if dir != nil {
		a.Directory = dir
	}
	if cfg.Business != nil {
		// The node refuses to start with a broken file, so this one loads;
		// its dates and notes are what questions to carriers must not carry.
		biz, err := business.Load(cfg.Business.File)
		if err != nil {
			return nil, err
		}
		a.Private = biz.Private()
	}
	return a, nil
}

// CarrierAssistant builds the carrier's assistant (the operator role's),
// for the browser UI: its tools read the carrier side's bookings and draft
// delays, which a person publishes in the confirmation dialog.
func CarrierAssistant(cfg *config.Config, node assistant.CarrierNode) (*assistant.Assistant, error) {
	if !cfg.Has(config.RoleOperator) {
		return nil, fmt.Errorf("%s has roles %v; the carrier's assistant needs the operator role", cfg.Domain, cfg.Roles)
	}
	if cfg.LLM == nil {
		return nil, errors.New("the configuration has no llm section: the assistant needs a model (provider: fake needs no key)")
	}
	model, err := NewModel(cfg.LLM, assistant.FakeCarrierModel())
	if err != nil {
		return nil, err
	}
	// The company's own skills (how it phrases delays, say) shape it too.
	var agentSkills []skills.Skill
	if cfg.Skills != nil {
		if agentSkills, err = skills.LoadDir(cfg.Skills.Dir); err != nil {
			return nil, err
		}
	}
	return &assistant.Assistant{Model: model, Carrier: node, Drafts: &assistant.Drafts{}, Skills: agentSkills,
		Self: assistant.Self{Domain: cfg.Domain, Name: cfg.Name}}, nil
}

// Partners loads the configured address book's entries and the directory
// to search, either absent if not configured.
func Partners(cfg *config.Config) ([]directory.Entry, *directory.File, error) {
	var book []directory.Entry
	if cfg.AddressBook != nil {
		f, err := directory.Load(cfg.AddressBook.File)
		if err != nil {
			return nil, nil, fmt.Errorf("addressBook: %w", err)
		}
		book = f.Entries()
	}
	var dir *directory.File
	if cfg.Directory != nil {
		var err error
		if dir, err = directory.Load(cfg.Directory.File); err != nil {
			return nil, nil, fmt.Errorf("directory: %w", err)
		}
	}
	return book, dir, nil
}
