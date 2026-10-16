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
//
//	ulpmock qualp --listen 127.0.0.1:8090 [--token-env QUALP_TOKEN]
//
// qualp is a stand-in for QualP, the Brazilian routing service: its route
// and ANTT freight-floor endpoints, in its own wire format, with simulated
// routes and the ANTT's published coefficients (internal/qualpmock).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"syscall"
	"time"

	"github.com/go-steer/supercargo/internal/edi"
	"github.com/go-steer/supercargo/internal/mockcarrier"
	"github.com/go-steer/supercargo/internal/qualpmock"
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
  ulpmock edifact --id ID --partner ID --in DIR --out DIR [--ecmr-prefix P] [--zone ZONE] [--hours-per-second N] [--tick D] [--clock trip|shared]
  ulpmock qualp --listen ADDR [--token-env NAME]`

var prefixPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,39}$`)

func run(ctx context.Context, args []string, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "edifact":
		return edifact(ctx, args[1:], stderr)
	case "qualp":
		return qualp(ctx, args[1:], stderr)
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
	clock := fs.String("clock", "trip", "trip (each trip's clock starts an hour before its pickup) or shared (one clock from the first instruction, as the nodes' simulator.clock: shared)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" || *partner == "" || *in == "" || *out == "" || *speed <= 0 || *speed > 3600 || *tick <= 0 {
		return errors.New("edifact: --id, --partner, --in and --out are required; --hours-per-second is above 0 and at most 3600, --tick positive")
	}
	if *ecmr != "" && !prefixPattern.MatchString(*ecmr) {
		return fmt.Errorf("edifact: --ecmr-prefix %q must be 1-40 of [A-Za-z0-9-], starting with a letter or digit", *ecmr)
	}
	if *clock != "trip" && *clock != "shared" {
		return errors.New("edifact: --clock is trip or shared")
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
	c.Shared = *clock == "shared"
	log.Info("ulpmock edifact", "id", *id, "partner", *partner, "in", *in, "out", *out)
	return c.Run(ctx, *tick)
}

// qualp serves the mock QualP until ctx is done. The token, if any, is read
// from an environment variable, never a flag (flags show in ps).
func qualp(ctx context.Context, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("qualp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "127.0.0.1:8090", "address to listen on")
	tokenEnv := fs.String("token-env", "", "environment variable holding the only Access-Token accepted (default: any non-empty one)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	srv := &qualpmock.Server{}
	if *tokenEnv != "" {
		if srv.Token = os.Getenv(*tokenEnv); srv.Token == "" {
			return fmt.Errorf("qualp: %s is empty", *tokenEnv)
		}
	}
	l, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 10 * time.Second, IdleTimeout: time.Minute}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	log.Info("ulpmock qualp", "listen", l.Addr().String(), "router", qualpmock.Router)
	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(l) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return hs.Shutdown(sctx) //nolint:contextcheck // the shutdown outlives the cancelled ctx
}
