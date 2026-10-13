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

// Command ulpmock runs the demo's stand-ins for companies without a ULP
// node (docs/proposals/backend-integration.md, "mock backends").
//
//	ulpmock edifact --id polder-haulage.example --partner meridian-forwarding.example \
//	    --in /var/edi/to-polder --out /var/edi/from-polder --ecmr-prefix ECMR-NL-2026
//
// edifact is a road carrier on a file drop: it acknowledges each transport
// instruction (IFTMIN) with an APERAK and reports the trip as IFTSTA
// statuses, on an accelerated clock.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-steer/supercargo/internal/edi"
	"github.com/go-steer/supercargo/internal/mockcarrier"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stderr)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ulpmock:", err)
		os.Exit(1)
	}
}

const usage = `usage:
  ulpmock edifact --id ID --partner ID --in DIR --out DIR [--ecmr-prefix P] [--zone ZONE] [--hours-per-second N]`

func run(ctx context.Context, args []string, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "edifact":
		return edifact(ctx, args[1:], stderr)
	default:
		return fmt.Errorf("unknown command %q\n%s", args[0], usage)
	}
}

func edifact(ctx context.Context, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("edifact", flag.ContinueOnError)
	fs.SetOutput(stderr)
	id := fs.String("id", "", "the carrier's own EDI identity (UNB sender), e.g. polder-haulage.example")
	partner := fs.String("partner", "", "the forwarder's EDI identity, whose instructions it takes")
	in := fs.String("in", "", "the directory instructions arrive in (the forwarder's outbox)")
	out := fs.String("out", "", "the directory its replies go to (the forwarder's inbox)")
	ecmr := fs.String("ecmr-prefix", "", "numbers an e-CMR for each trip, e.g. ECMR-NL-2026")
	zone := fs.String("zone", "UTC", "the time zone agreed for EDI times, e.g. Europe/Amsterdam")
	speed := fs.Float64("hours-per-second", 1, "the trips' clock speed")
	tick := fs.Duration("tick", 500*time.Millisecond, "how often the drop is checked")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" || *partner == "" || *in == "" || *out == "" || *speed <= 0 || *tick <= 0 {
		return errors.New("edifact: --id, --partner, --in and --out are required; --hours-per-second and --tick are positive")
	}
	loc, err := time.LoadLocation(*zone)
	if err != nil {
		return fmt.Errorf("edifact: --zone: %w", err)
	}
	inDir, err := edi.NewDir(*in)
	if err != nil {
		return err
	}
	defer func() { _ = inDir.Close() }()
	outDir, err := edi.NewDir(*out)
	if err != nil {
		return err
	}
	defer func() { _ = outDir.Close() }()
	log := slog.New(slog.NewTextHandler(stderr, nil))
	c := &mockcarrier.EDIFACT{ID: *id, Partner: *partner, In: inDir, Out: outDir, ECMRPrefix: *ecmr, Zone: loc,
		HoursPerSecond: *speed, Now: time.Now, Log: log}
	log.Info("ulpmock edifact", "id", *id, "partner", *partner, "in", *in, "out", *out)
	return c.Run(ctx, *tick)
}
