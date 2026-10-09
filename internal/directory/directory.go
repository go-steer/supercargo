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

// Package directory finds partners (docs/DESIGN.md, A.3): a company's own
// address book of the partners it knows, and directories to search for
// ones it doesn't. Neither is trust. An entry only names a domain and what
// it claims; the profile at that domain, fetched and verified, is what
// counts, and an entry can never vouch for a key or an endpoint.
//
// The file format is the same for both: a list of entries.
//
//	entries:
//	  - domain: ocean-blue.example
//	    name: Ocean Blue
//	    roles: [carrier]
//	    modes: [OCEAN]
//	    lanes: [{ from: NLRTM, to: USEWR }]
//	    description: Transatlantic container service, weekly.
package directory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"

	"github.com/go-steer/supercargo/pkg/envelope"
)

// Roles a partner can have.
const (
	RoleCarrier   = "carrier"
	RoleShipper   = "shipper"
	RoleConsignee = "consignee"
)

// Limits.
const (
	MaxFile        = 256 << 10
	MaxEntries     = 500
	MaxName        = 128
	MaxDescription = 500
	MaxLanes       = 50
	// MaxResults bounds what one search returns.
	MaxResults = 10
)

// Entry is one partner as a directory lists it. Name and Description are
// written by whoever wrote the entry: text to show as text.
type Entry struct {
	Domain string   `yaml:"domain" json:"domain"`
	Name   string   `yaml:"name" json:"name"`
	Roles  []string `yaml:"roles" json:"roles"`
	// Modes are the transport modes a carrier runs (Modes), if it says.
	Modes       []string `yaml:"modes" json:"modes,omitempty"`
	Lanes       []Lane   `yaml:"lanes" json:"lanes,omitempty"`
	Description string   `yaml:"description" json:"description,omitempty"`
}

// Modes, as ULP's payload schemas name them.
var Modes = []string{"OCEAN", "ROAD", "RAIL", "AIR"}

// Lane is a port pair a carrier serves, as UN/LOCODEs.
type Lane struct {
	From string `yaml:"from" json:"from"`
	To   string `yaml:"to" json:"to"`
}

// Query is a search. Every field set must match: Role one of the entry's
// roles, Mode one of its modes (an entry with no modes matches any mode),
// From and To one of its lanes (an entry with no lanes matches any
// lane), Text a part of its name, domain or description (case folded).
type Query struct {
	Role string
	Mode string
	From string
	To   string
	Text string
}

// Directory is something to search for partners.
type Directory interface {
	Search(ctx context.Context, q Query) ([]Entry, error)
}

// File is a directory read from a file: a company's address book, or a
// static directory for local runs and tests.
type File struct {
	entries []Entry
}

// Entries are the file's entries, in the file's order.
func (f *File) Entries() []Entry { return slices.Clone(f.entries) }

// Search implements Directory.
func (f *File) Search(_ context.Context, q Query) ([]Entry, error) {
	text := strings.ToLower(strings.TrimSpace(q.Text))
	// Entries that list the lane come before ones that list no lanes (and
	// so match any): a catch-all entry can't crowd out a carrier that
	// says it serves the lane.
	var out, catchAll []Entry
	for _, e := range f.entries {
		if q.Role != "" && !slices.Contains(e.Roles, q.Role) {
			continue
		}
		if q.Mode != "" && len(e.Modes) > 0 && !slices.Contains(e.Modes, q.Mode) {
			continue
		}
		if (q.From != "" || q.To != "") && len(e.Lanes) > 0 && !slices.ContainsFunc(e.Lanes, func(l Lane) bool {
			return (q.From == "" || l.From == q.From) && (q.To == "" || l.To == q.To)
		}) {
			continue
		}
		if text != "" && !strings.Contains(strings.ToLower(e.Name+" "+e.Domain+" "+e.Description), text) {
			continue
		}
		if (q.From != "" || q.To != "") && len(e.Lanes) == 0 {
			catchAll = append(catchAll, e)
		} else {
			out = append(out, e)
		}
	}
	out = append(out, catchAll...)
	if len(out) > MaxResults {
		out = out[:MaxResults]
	}
	return out, nil
}

// Load reads and validates a directory file. Loading is strict, as for the
// node's config: an unknown key is an error.
func Load(path string) (*File, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the company's own configured file
	if err != nil {
		return nil, fmt.Errorf("directory: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, MaxFile+1))
	if err != nil {
		return nil, fmt.Errorf("directory: %w", err)
	}
	if len(data) > MaxFile {
		return nil, fmt.Errorf("directory: %s is over %d bytes", path, MaxFile)
	}
	return Parse(data)
}

// Parse decodes and validates a directory file.
func Parse(data []byte) (*File, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var doc struct {
		Entries []Entry `yaml:"entries"`
	}
	if err := dec.Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("directory: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("directory: only one YAML document is allowed")
	}
	if len(doc.Entries) > MaxEntries {
		return nil, fmt.Errorf("directory: more than %d entries", MaxEntries)
	}
	seen := map[string]bool{}
	for i := range doc.Entries {
		e := &doc.Entries[i]
		if err := e.Validate(); err != nil {
			return nil, fmt.Errorf("directory: entries[%d]: %w", i, err)
		}
		if seen[e.Domain] {
			return nil, fmt.Errorf("directory: entries[%d]: %s appears twice", i, e.Domain)
		}
		seen[e.Domain] = true
	}
	return &File{entries: doc.Entries}, nil
}

var unLocode = regexp.MustCompile(`^[A-Z]{2}[A-Z2-9]{3}$`)

// ValidLocode reports whether s is shaped like a UN/LOCODE (NLRTM).
func ValidLocode(s string) bool { return unLocode.MatchString(s) }

// Validate checks an entry as loading a file does.
func (e *Entry) Validate() error {
	if !envelope.ValidDomain(e.Domain) {
		return fmt.Errorf("domain %q is not a domain", e.Domain)
	}
	if err := text("name", e.Name, MaxName); err != nil || strings.TrimSpace(e.Name) == "" {
		if err == nil {
			err = errors.New("name is empty")
		}
		return err
	}
	if err := text("description", e.Description, MaxDescription); err != nil {
		return err
	}
	if len(e.Roles) == 0 {
		return errors.New("no roles")
	}
	for _, r := range e.Roles {
		if r != RoleCarrier && r != RoleShipper && r != RoleConsignee {
			return fmt.Errorf("role %q is not %s, %s or %s", r, RoleCarrier, RoleShipper, RoleConsignee)
		}
	}
	for _, m := range e.Modes {
		if !slices.Contains(Modes, m) {
			return fmt.Errorf("mode %q is not one of %v", m, Modes)
		}
	}
	if len(e.Lanes) > MaxLanes {
		return fmt.Errorf("more than %d lanes", MaxLanes)
	}
	for _, l := range e.Lanes {
		if !unLocode.MatchString(l.From) || !unLocode.MatchString(l.To) {
			return fmt.Errorf("lane %s-%s: not UN/LOCODEs", l.From, l.To)
		}
	}
	return nil
}

// text refuses text that isn't one line of printable UTF-8 of at most max
// bytes: no control or format characters (bidi overrides, zero-width).
func text(field, s string, max int) error {
	if len(s) > max || !utf8.ValidString(s) {
		return fmt.Errorf("%s: over %d bytes, or not UTF-8", field, max)
	}
	for _, r := range s {
		if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) {
			return fmt.Errorf("%s: a control or format character (%U)", field, r)
		}
	}
	return nil
}

// NameKey is a name as names are compared: letters and digits, case
// folded, every run of anything else one space. "Ocean Blue, Inc." and
// "ocean blue inc" have the same key.
func NameKey(s string) string {
	var b strings.Builder
	space := false
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteRune(r)
			space = false
		} else {
			space = true
		}
	}
	return b.String()
}
