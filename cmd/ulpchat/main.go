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

// Command ulpchat is the shipper's chat (docs/DESIGN.md, A.10, LLM #1):
// a full-screen TUI over the shipper assistant, which works through the
// running node's control socket.
//
//	ulpchat --config shipper.yaml [--theme dark|light]
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/go-steer/supercargo/internal/app"
	"github.com/go-steer/supercargo/internal/assistant"
	"github.com/go-steer/supercargo/internal/business"
	"github.com/go-steer/supercargo/internal/chatui"
	"github.com/go-steer/supercargo/internal/config"
	"github.com/go-steer/supercargo/internal/control"
	"github.com/go-steer/supercargo/internal/skills"
	"github.com/go-steer/supercargo/internal/ui/theme"
)

func main() {
	if err := run(os.Args[1:], os.Stderr, func(m tea.Model) error {
		_, err := tea.NewProgram(m).Run()
		return err
	}); err != nil {
		fmt.Fprintln(os.Stderr, "ulpchat:", err)
		os.Exit(1)
	}
}

func run(args []string, stderr io.Writer, start func(tea.Model) error) error {
	fs := flag.NewFlagSet("ulpchat", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "", "the shipper node's configuration (YAML): its control socket, llm and skills are used")
	themeName := fs.String("theme", "", "dark or light (default: follow the terminal's background)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" {
		return errors.New("--config is required")
	}
	if *themeName != "" {
		if _, ok := theme.ByName(*themeName); !ok {
			return fmt.Errorf("--theme %q is not dark or light", *themeName)
		}
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	a, err := newAssistant(cfg)
	if err != nil {
		return err
	}
	return start(chatui.New(chatui.Options{Assistant: a, Theme: *themeName, Node: cfg.Control.Socket}))
}

// newAssistant builds the assistant for a shipper node's configuration.
func newAssistant(cfg *config.Config) (*assistant.Assistant, error) {
	if !cfg.Has(config.RoleShipper) {
		return nil, fmt.Errorf("%s has roles %v; ulpchat is the shipper's chat so far, and needs the shipper role (#75)", cfg.Domain, cfg.Roles)
	}
	if cfg.Control == nil {
		return nil, errors.New("the configuration has no control socket, which ulpchat talks to the node through")
	}
	if cfg.LLM == nil {
		return nil, errors.New("the configuration has no llm section: the assistant needs a model (provider: fake needs no key)")
	}
	model, err := app.NewModel(cfg.LLM, assistant.FakeModel(time.Now))
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
	a := &assistant.Assistant{Model: model, Node: assistant.ControlNode{Client: control.Client(cfg.Control.Socket, 2*time.Minute)},
		Drafts: &assistant.Drafts{}, Skills: agentSkills,
		Self: assistant.Self{Domain: cfg.Domain, Name: cfg.Name, Endpoint: app.RESTURL(cfg)}}
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
