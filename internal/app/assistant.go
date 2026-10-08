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
	a := &assistant.Assistant{Model: model, Node: node, Drafts: &assistant.Drafts{}, Skills: agentSkills,
		Self: assistant.Self{Domain: cfg.Domain, Name: cfg.Name, Endpoint: RESTURL(cfg)}}
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
