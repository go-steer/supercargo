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
	voyage := fs.String("voyage", "", "instead of one booking, every booking under way on this voyage, e.g. 42W")
	vessel := fs.String("vessel", "", "with --voyage: the vessel, if voyage numbers repeat across vessels")
	delay := fs.Duration("delay", 0, "how much later the vessel arrives, e.g. 72h")
	reason := fs.String("reason", "", "why, in one line (it is the alert's summary)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" || (*booking == "") == (*voyage == "") || *delay <= 0 || *reason == "" {
		return errors.New("disrupt: --config, --booking or --voyage (one), a positive --delay and --reason are required")
	}
	if *vessel != "" && *voyage == "" {
		return errors.New("disrupt: --vessel goes with --voyage")
	}
	socket, err := operatorSocket(*path)
	if err != nil {
		return err
	}
	var dr operator.DisruptResponse
	code, err := control.Call(control.Client(socket, 10*time.Second), "/disrupt",
		operator.DisruptRequest{BookingRef: *booking, Vessel: *vessel, Voyage: *voyage, Delay: delay.String(), Reason: *reason}, &dr)
	if err != nil {
		return fmt.Errorf("disrupt: %w", err)
	}
	if *voyage != "" {
		if code != http.StatusOK {
			return fmt.Errorf("disrupt: HTTP %d: %s", code, dr.Error)
		}
		fmt.Fprintf(stdout, "Disrupted voyage %s: %d booking(s) under way.\n", *voyage, len(dr.Alerts))
		failed, applied := 0, 0
		for _, va := range dr.Alerts {
			if va.Alert == nil {
				failed++
				if va.Applied {
					applied++
					fmt.Fprintf(stdout, "  %s: DELAYED, but no alert sent: %s\n", va.BookingRef, va.Error)
				} else {
					fmt.Fprintf(stdout, "  %s: not delayed: %s\n", va.BookingRef, va.Error)
				}
				continue
			}
			fmt.Fprintf(stdout, "  %s: %s %s alert %s, ETA %s → %s\n", va.BookingRef, va.Alert.Severity, va.Alert.Kind, va.Alert.AlertID,
				va.Alert.PreviousETA, va.Alert.NewETA)
		}
		if applied > 0 {
			return fmt.Errorf("disrupt: %d of %d bookings got no alert, %d of them delayed anyway: don't run this again, it would delay them twice", failed, len(dr.Alerts), applied)
		}
		if failed > 0 {
			return fmt.Errorf("disrupt: %d of %d bookings got no alert and were not delayed", failed, len(dr.Alerts))
		}
		return nil
	}
	if code == http.StatusOK && dr.Applied {
		fmt.Fprintf(stdout, "Delayed %s, but its alert was not sent: %s\n", *booking, dr.Error)
		return errors.New("disrupt: the delay is applied without its alert; don't run this again, it would delay the voyage twice")
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
	if !cfg.Has(config.RoleOperator) {
		return "", fmt.Errorf("disrupt: %s has no operator role", path)
	}
	if cfg.Control == nil {
		return "", fmt.Errorf("disrupt: %s has no control socket", path)
	}
	return cfg.Control.Socket, nil
}
