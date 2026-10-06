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

// Command ulpnode runs one company's ULP node (docs/DESIGN.md, section A.1).
//
//	ulpnode serve  --config node.yaml               run the node
//	ulpnode keygen --kid cy-2026-1 --out key.pem    mint a signing key; print its public JWK
//	ulpnode book   --config node.yaml --operator ocean-blue.example --request booking.json
//	                                                send a booking (after an explicit y)
//	ulpnode ask    --config node.yaml --booking OB-BKG-1042 "Why the delay?"
//	                                                ask the booking's operator a question
package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-steer/supercargo/internal/app"
	"github.com/go-steer/supercargo/internal/config"
	"github.com/go-steer/supercargo/internal/control"
	"github.com/go-steer/supercargo/internal/shipper"
	"github.com/go-steer/supercargo/pkg/intents"
	"github.com/go-steer/supercargo/pkg/sig"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "ulpnode:", err)
		os.Exit(1)
	}
}

const usage = `usage:
  ulpnode serve  --config FILE [--log-format text|json]
  ulpnode keygen --kid KID --out FILE
  ulpnode ask    --config FILE --booking REF [--operator DOMAIN] QUESTION...
  ulpnode book   --config FILE --operator DOMAIN --request FILE [--yes]`

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "serve":
		return serve(args[1:], stderr)
	case "keygen":
		return keygen(args[1:], stdout)
	case "book":
		return book(args[1:], stdin, stdout, stderr)
	case "ask":
		return ask(args[1:], stdout, stderr)
	default:
		return fmt.Errorf("unknown command %q\n%s", args[0], usage)
	}
}

func logger(format string, w io.Writer) (*slog.Logger, error) {
	switch format {
	case "text":
		return slog.New(slog.NewTextHandler(w, nil)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, nil)), nil
	}
	return nil, fmt.Errorf("--log-format %q is not text or json", format)
}

func serve(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "", "node configuration (YAML)")
	format := fs.String("log-format", "text", "text or json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" {
		return errors.New("serve: --config is required")
	}
	log, err := logger(*format, stderr)
	if err != nil {
		return err
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	a, err := app.New(cfg, log)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return a.Serve(ctx)
}

func keygen(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	kid := fs.String("kid", "", "key ID to publish the key under")
	out := fs.String("out", "", "where to write the private key (PKCS#8 PEM, mode 0600; must not exist)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !sig.ValidKID(*kid) || *out == "" {
		return errors.New("keygen: --kid (1-64 of [A-Za-z0-9._-]) and --out are required")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	if err := config.WriteSigningKey(*out, priv); err != nil {
		return err
	}
	jwk, err := sig.Ed25519JWK(pub, *kid)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "%s\n", jwk)
	return err
}

func book(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("book", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "", "the shipper node's configuration (YAML)")
	operatorDomain := fs.String("operator", "", "the operator's domain")
	reqPath := fs.String("request", "", "the booking request body (JSON)")
	yes := fs.Bool("yes", false, "send without asking (scripts; the person running it is the confirmation)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" || *operatorDomain == "" || *reqPath == "" {
		return errors.New("book: --config, --operator and --request are required")
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	if cfg.Role != config.RoleShipper {
		return errors.New("book: the config is not a shipper's")
	}
	// The booking goes through the running node, which records it and so
	// accepts the operator's pushes for it.
	if cfg.Control == nil {
		return errors.New("book: the config has no control socket; book through a running node (add control: { socket: ... })")
	}
	req, err := readRequest(*reqPath)
	if err != nil {
		return err
	}
	// Checked before asking: a node that isn't running shows up before
	// anyone says yes, not after.
	if fi, err := os.Stat(cfg.Control.Socket); err != nil || fi.Mode().Type() != os.ModeSocket {
		return fmt.Errorf("book: no node listening on %s: start it with ulpnode serve", cfg.Control.Socket)
	}
	printSummary(stdout, *operatorDomain, req)
	if !*yes {
		// The hard rule: an action with external effect needs an explicit
		// yes from a person.
		fmt.Fprint(stdout, "Send this booking request? [y/N] ")
		answer, _ := bufio.NewReader(stdin).ReadString('\n')
		if strings.TrimSpace(strings.ToLower(answer)) != "y" {
			fmt.Fprintln(stdout, "Not sent.")
			return errNotSent
		}
	}
	var resp shipper.BookResponse
	// Longer than the node's write timeout, so the node gives up first.
	code, err := control.Call(control.Client(cfg.Control.Socket, 75*time.Second), "/book",
		shipper.BookRequest{Operator: *operatorDomain, Request: *req}, &resp)
	if errors.Is(err, control.ErrNoAnswer) {
		return fmt.Errorf("book: %w; check the node's log before booking again", err)
	}
	if err != nil {
		return fmt.Errorf("book: %w", err)
	}
	if code != http.StatusOK || resp.Confirmation == nil {
		return fmt.Errorf("book: HTTP %d: %s", code, resp.Error)
	}
	out, _ := json.MarshalIndent(resp.Confirmation, "", "  ")
	_, err = fmt.Fprintf(stdout, "%s\n", out)
	return err
}

// errNotSent reports a booking the person declined: a non-zero exit, so a
// script can tell "not sent" from "sent".
var errNotSent = errors.New("book: not sent")

// printSummary shows everything the operator will learn or act on, so a
// yes is an informed one: where events will be pushed, who the consignee
// is, what is shipped.
func printSummary(w io.Writer, operator string, r *intents.BookingRequestBody) {
	fmt.Fprintf(w, "Booking request to %s (ref %s):\n", operator, r.ShipperRef)
	fmt.Fprintf(w, "  Route:      %s (%s) → %s (%s), earliest departure %s\n",
		r.Origin.Name, r.Origin.UNLocode, r.Destination.Name, r.Destination.UNLocode, r.EarliestDeparture)
	for _, e := range r.Equipment {
		fmt.Fprintf(w, "  Equipment:  %d × %s (%s)\n", e.Count, e.Label, e.ISOType)
	}
	fmt.Fprintf(w, "  Cargo:      %s, HS %s\n", r.Cargo.Description, r.Cargo.HSCode)
	if r.Consignee != nil {
		fmt.Fprintf(w, "  Consignee:  %s (%s)\n", r.Consignee.Name, r.Consignee.Domain)
	}
	if r.Subscription != nil {
		fmt.Fprintf(w, "  Updates:    %s will push %s to %s\n", operator, strings.Join(r.Subscription.Intents, ", "), r.Subscription.Endpoint)
	} else {
		fmt.Fprintln(w, "  Updates:    none requested")
	}
}

func readRequest(path string) (*intents.BookingRequestBody, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the user's own request file
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(io.LimitReader(f, 64<<10))
	dec.DisallowUnknownFields()
	var req intents.BookingRequestBody
	if err := dec.Decode(&req); err != nil {
		return nil, fmt.Errorf("book: %s: %w", path, err)
	}
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("book: %s: %w", path, err)
	}
	return &req, nil
}

// ask sends an inquiry through the running shipper node and prints the
// operator's answer, marking the cited records this shipper holds signed.
func ask(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "", "the shipper node's configuration (YAML)")
	booking := fs.String("booking", "", "the booking reference")
	operatorDomain := fs.String("operator", "", "the operator (needed only if several have that reference)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	question := strings.Join(fs.Args(), " ")
	if *path == "" || *booking == "" || question == "" {
		return errors.New("ask: --config, --booking and a question are required")
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	if cfg.Role != config.RoleShipper || cfg.Control == nil {
		return errors.New("ask: the config must be a shipper's, with a control socket")
	}
	var resp shipper.AskResponse
	code, err := control.Call(control.Client(cfg.Control.Socket, 75*time.Second), "/ask",
		shipper.AskRequest{Operator: *operatorDomain, BookingRef: *booking, Question: question}, &resp)
	if err != nil {
		return fmt.Errorf("ask: %w", err)
	}
	if code != http.StatusOK || resp.Answer == nil {
		return fmt.Errorf("ask: HTTP %d: %s", code, resp.Error)
	}
	a := resp.Answer
	fmt.Fprintf(stdout, "%s answered (%s, from %d records of %s):\n\n", resp.Operator, a.GeneratedBy, a.DataScope.EventsConsidered, a.DataScope.BookingRef)
	for _, line := range strings.Split(a.Answer, "\n") {
		fmt.Fprintf(stdout, "  %s\n", line)
	}
	if len(a.CitedEventIDs) > 0 {
		held := map[string]bool{}
		for _, id := range resp.Held {
			held[id] = true
		}
		var sources []string
		for _, id := range a.CitedEventIDs {
			if held[id] {
				sources = append(sources, id+" (held, signed)")
			} else {
				sources = append(sources, id+" (not held)")
			}
		}
		fmt.Fprintf(stdout, "\n  Sources: %s\n", strings.Join(sources, ", "))
	}
	return nil
}
