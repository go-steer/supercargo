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
	"context"

	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"go.opentelemetry.io/otel"

	tea "charm.land/bubbletea/v2"

	"github.com/go-steer/supercargo/internal/app"
	"github.com/go-steer/supercargo/internal/assistant"
	"github.com/go-steer/supercargo/internal/chatui"
	"github.com/go-steer/supercargo/internal/config"
	"github.com/go-steer/supercargo/internal/control"
	"github.com/go-steer/supercargo/internal/telemetry"
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
	// The assistant's turns, model calls and tool calls export with the
	// node's telemetry settings, as their own service. Export errors are
	// dropped: the TUI owns the terminal.
	shutdown, err := telemetry.Setup(context.Background(), cfg.Telemetry, "ulpchat ("+cfg.Domain+")")
	if err != nil {
		return fmt.Errorf("telemetry: %w", err)
	}
	if telemetry.Enabled(cfg.Telemetry, os.Getenv) {
		otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) {}))
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdown(ctx)
	}()
	return start(chatui.New(chatui.Options{Assistant: a, Theme: *themeName, Node: cfg.Control.Socket}))
}

// newAssistant builds the assistant for a shipper node's configuration,
// talking to the running node over its control socket.
func newAssistant(cfg *config.Config) (*assistant.Assistant, error) {
	if !cfg.Has(config.RoleShipper) {
		return nil, fmt.Errorf("%s has roles %v; ulpchat is the shipper's chat so far, and needs the shipper role (#75)", cfg.Domain, cfg.Roles)
	}
	if cfg.Control == nil {
		return nil, errors.New("the configuration has no control socket, which ulpchat talks to the node through")
	}
	return app.ShipperAssistant(cfg, assistant.ControlNode{Client: control.Client(cfg.Control.Socket, 2*time.Minute)})
}
