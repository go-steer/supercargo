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

// Package business is a shipper's private business context (docs/DESIGN.md,
// A.9): required delivery dates, warehouse receiving slots, launch notes.
// The shipper's own model uses it to judge what a delay means. It never
// leaves the shipper: nothing here is ever put in an envelope, and Guard
// refuses any outbound body that would carry its text.
package business

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

// Limits.
const (
	MaxShipments = 1000
	MaxText      = 500
	MaxFile      = 256 << 10
)

// Context is the business context file.
type Context struct {
	// Shipments are per-shipment constraints, matched to a booking by the
	// shipper's own reference (the booking request's shipperRef).
	Shipments []Shipment `yaml:"shipments"`
	// Notes are general notes for the assistant (company-wide).
	Notes []string `yaml:"notes"`
}

// Shipment is what one shipment must achieve, privately.
type Shipment struct {
	ShipperRef    string `yaml:"shipperRef"`
	RequiredBy    string `yaml:"requiredBy"` // YYYY-MM-DD: the latest acceptable delivery
	ReceivingSlot *Slot  `yaml:"receivingSlot"`
	Notes         string `yaml:"notes"`
}

// Slot is a warehouse receiving appointment.
type Slot struct {
	Date      string `yaml:"date"`   // YYYY-MM-DD
	Window    string `yaml:"window"` // e.g. "08:00-12:00"
	Warehouse string `yaml:"warehouse"`
}

var (
	// refPattern is a shipper reference as a booking request may carry it
	// (intents' shipper_reference): no dots.
	refPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	windowPattern = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d-([01]\d|2[0-3]):[0-5]\d$`)
)

// Load reads and validates a business context file. Loading is strict, as
// for the node's config: an unknown key is an error.
func Load(path string) (*Context, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the shipper's own configured file
	if err != nil {
		return nil, fmt.Errorf("business: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, MaxFile+1))
	if err != nil {
		return nil, fmt.Errorf("business: %w", err)
	}
	if len(data) > MaxFile {
		return nil, fmt.Errorf("business: %s is over %d bytes", path, MaxFile)
	}
	return Parse(data)
}

// Parse decodes and validates a business context.
func Parse(data []byte) (*Context, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var c Context
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("business: %w", err)
	}
	// A second YAML document would be silently ignored; refuse it, as the
	// node's config does.
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("business: only one YAML document is allowed")
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("business: %w", err)
	}
	return &c, nil
}

func (c *Context) validate() error {
	if len(c.Shipments) > MaxShipments {
		return fmt.Errorf("more than %d shipments", MaxShipments)
	}
	seen := map[string]bool{}
	for i, s := range c.Shipments {
		at := fmt.Sprintf("shipments[%d]", i)
		if !refPattern.MatchString(s.ShipperRef) {
			return fmt.Errorf("%s.shipperRef %q is not 1-64 of [A-Za-z0-9_-], starting with a letter or digit", at, s.ShipperRef)
		}
		if seen[s.ShipperRef] {
			return fmt.Errorf("%s.shipperRef %s appears twice", at, s.ShipperRef)
		}
		seen[s.ShipperRef] = true
		if s.RequiredBy != "" {
			if err := date(at+".requiredBy", s.RequiredBy); err != nil {
				return err
			}
		}
		if s.ReceivingSlot != nil {
			if err := date(at+".receivingSlot.date", s.ReceivingSlot.Date); err != nil {
				return err
			}
			if s.ReceivingSlot.Window != "" && !windowPattern.MatchString(s.ReceivingSlot.Window) {
				return fmt.Errorf("%s.receivingSlot.window %q is not HH:MM-HH:MM", at, s.ReceivingSlot.Window)
			}
			if err := text(at+".receivingSlot.warehouse", s.ReceivingSlot.Warehouse); err != nil {
				return err
			}
		}
		if err := text(at+".notes", s.Notes); err != nil {
			return err
		}
	}
	for i, n := range c.Notes {
		if err := text(fmt.Sprintf("notes[%d]", i), n); err != nil {
			return err
		}
	}
	return nil
}

func date(field, s string) error {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil || t.Format(time.DateOnly) != s {
		return fmt.Errorf("%s %q is not YYYY-MM-DD", field, s)
	}
	return nil
}

// text checks free text: it reaches a model, so the same rules as a
// message's free text (no control or format characters), and a cap.
func text(field, s string) error {
	if utf8.RuneCountInString(s) > MaxText {
		return fmt.Errorf("%s is over %d characters", field, MaxText)
	}
	for _, r := range s {
		if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Co, unicode.Cs) || r == 0x2028 || r == 0x2029 || r == utf8.RuneError {
			return fmt.Errorf("%s contains %U, which may not appear", field, r)
		}
	}
	return nil
}

// For returns the constraints for the shipment the shipper calls ref.
func (c *Context) For(shipperRef string) (Shipment, bool) {
	if c == nil {
		return Shipment{}, false
	}
	for _, s := range c.Shipments {
		if s.ShipperRef == shipperRef {
			return s, true
		}
	}
	return Shipment{}, false
}

// GuardMinLen is the shortest text Guard looks for: shorter strings (and
// dates, which legitimately appear in bookings) would match by accident.
const GuardMinLen = 8

// Secrets are the context's free-text values Guard looks for.
func (c *Context) Secrets() []string {
	if c == nil {
		return nil
	}
	var out []string
	add := func(s string) {
		if s = strings.TrimSpace(s); utf8.RuneCountInString(s) >= GuardMinLen {
			out = append(out, s)
		}
	}
	for _, s := range c.Shipments {
		add(s.Notes)
		if s.ReceivingSlot != nil {
			add(s.ReceivingSlot.Warehouse)
			add(s.ReceivingSlot.Window) // "08:00-12:00": distinctive enough to look for
		}
	}
	for _, n := range c.Notes {
		add(n)
	}
	return out
}

// Private is everything a message to a partner must not mention: the
// Secrets, and the required-by and receiving-slot dates (which the guard
// can't look for, so the assistant's own check does).
func (c *Context) Private() []string {
	if c == nil {
		return nil
	}
	out := c.Secrets()
	for _, s := range c.Shipments {
		if s.RequiredBy != "" {
			out = append(out, s.RequiredBy)
		}
		if s.ReceivingSlot != nil && s.ReceivingSlot.Date != "" {
			out = append(out, s.ReceivingSlot.Date)
		}
	}
	return out
}

// ErrLeak reports an outbound body that carries private context.
var ErrLeak = errors.New("business: an outbound message would carry private business context")

// Guard returns a check for outbound bodies: it refuses one containing any
// of the context's free text (and receiving windows). It's defence in
// depth: nothing builds an envelope from the context, and this catches a
// verbatim echo (a model copying a note into a question, say). It can't
// catch a paraphrase, a change of case, or the dates, which legitimately
// appear in bookings; keeping those out rests on nothing ever building an
// envelope from the context.
func (c *Context) Guard() func(body []byte) error {
	// A body is JSON: look for each secret as written and as encoding/json
	// writes it in a string (it escapes <, >, & and quotes).
	var needles [][]byte
	add := func(n []byte) {
		for _, have := range needles {
			if bytes.Equal(have, n) {
				return
			}
		}
		needles = append(needles, n)
	}
	for _, s := range c.Secrets() {
		add([]byte(s))
		// As JSON writes it in a string, with and without HTML escaping.
		for _, html := range []bool{true, false} {
			var buf bytes.Buffer
			enc := json.NewEncoder(&buf)
			enc.SetEscapeHTML(html)
			if enc.Encode(s) == nil {
				b := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
				add(b[1 : len(b)-1])
			}
		}
	}
	return func(body []byte) error {
		for _, n := range needles {
			if bytes.Contains(body, n) {
				return ErrLeak
			}
		}
		return nil
	}
}
