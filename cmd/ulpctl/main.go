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

// Command ulpctl is the presenter's console (docs/DESIGN.md, A.1). It
// drives a running operator node through its local control socket.
//
//	ulpctl disrupt --config operator.yaml --booking OB-BKG-1042 --delay 72h --reason "engine repair"
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-steer/supercargo/internal/config"
	"github.com/go-steer/supercargo/internal/control"
	"github.com/go-steer/supercargo/internal/operator"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "ulpctl:", err)
		os.Exit(1)
	}
}

const usage = `usage:
  ulpctl disrupt --config FILE --booking REF --delay DURATION --reason TEXT`

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "disrupt":
		return disrupt(args[1:], stdout, stderr)
	default:
		return fmt.Errorf("unknown command %q\n%s", args[0], usage)
	}
}

func disrupt(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("disrupt", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "", "the operator node's configuration (YAML); its control socket is used")
	booking := fs.String("booking", "", "the booking reference, e.g. OB-BKG-1042")
	delay := fs.Duration("delay", 0, "how much later the vessel arrives, e.g. 72h")
	reason := fs.String("reason", "", "why, in one line (it is the alert's summary)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" || *booking == "" || *delay <= 0 || *reason == "" {
		return errors.New("disrupt: --config, --booking, a positive --delay and --reason are required")
	}
	socket, err := operatorSocket(*path)
	if err != nil {
		return err
	}
	var dr operator.DisruptResponse
	code, err := control.Call(control.Client(socket, 10*time.Second), "/disrupt",
		operator.DisruptRequest{BookingRef: *booking, Delay: delay.String(), Reason: *reason}, &dr)
	if err != nil {
		return fmt.Errorf("disrupt: %w", err)
	}
	if code != http.StatusOK || dr.Alert == nil {
		return fmt.Errorf("disrupt: HTTP %d: %s", code, dr.Error)
	}
	a := dr.Alert
	fmt.Fprintf(stdout, "Disrupted %s: %s %s alert %s published.\n", *booking, a.Severity, a.Kind, a.AlertID)
	fmt.Fprintf(stdout, "  ETA:    %s → %s\n", a.PreviousETA, a.NewETA)
	fmt.Fprintf(stdout, "  Reason: %s\n", a.Summary)
	fmt.Fprintf(stdout, "  Cause:  %s\n", strings.Join(a.CauseEventIDs, ", "))
	return nil
}

// operatorSocket is the control socket of the operator node configured at
// path.
func operatorSocket(path string) (string, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return "", err
	}
	if cfg.Role != config.RoleOperator {
		return "", fmt.Errorf("disrupt: %s is not an operator's config", path)
	}
	if cfg.Control == nil {
		return "", fmt.Errorf("disrupt: %s has no control socket", path)
	}
	return cfg.Control.Socket, nil
}
