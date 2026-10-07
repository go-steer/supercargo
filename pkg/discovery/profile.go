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

// Package discovery is how one ULP party learns about another: the profile
// each publishes at https://{domain}/.well-known/ulp (docs/DESIGN.md,
// section A.3), fetched safely, validated, and cached with its signing keys.
//
// TLS is what ties a profile to its domain. The profile carries the keys
// that verify the domain's envelopes, and the endpoints that receive its
// traffic, so it is checked as strictly as a message.
package discovery

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/internal/logsafe"
	"github.com/go-steer/supercargo/pkg/internal/strictjson"
	"github.com/go-steer/supercargo/pkg/sig"
)

const (
	// Path is where a party publishes its profile.
	Path = "/.well-known/ulp"

	// MaxProfileSize is the largest profile accepted, in bytes.
	MaxProfileSize = 64 << 10

	// MaxKeys bounds the keys one profile may publish.
	MaxKeys = 16

	// MaxCapabilities, MaxTags and MaxDialects bound the optional
	// capabilities, each one's tags, and supported_dialects.
	MaxCapabilities = 32
	MaxTags         = 32
	MaxDialects     = 16
)

// Roles a party may have.
const (
	RoleOperator = "operator"
	RoleShipper  = "shipper"
)

// Profile is the published document. Members this version doesn't define
// are ignored, so profiles can grow; members that differ from a defined one
// only in case are rejected.
//
// Capabilities and SupportedDialects are optional, and named as in the
// ULP payload specification's discovery document (snake_case); the rest
// of the profile is ours.
type Profile struct {
	ProtocolVersion   string            `json:"protocolVersion"`
	Party             Party             `json:"party"`
	Services          []Service         `json:"services"`
	Capabilities      []Capability      `json:"capabilities,omitempty"`
	SupportedDialects []string          `json:"supported_dialects,omitempty"`
	Transports        Transports        `json:"transports"`
	Keys              []json.RawMessage `json:"keys"`
}

// Party names the publisher.
type Party struct {
	Domain string   `json:"domain"`
	Name   string   `json:"name"`
	Roles  []string `json:"roles"`
}

// Service lists the intents a party accepts.
type Service struct {
	Name       string   `json:"name"`
	Intents    []string `json:"intents"`
	Extensions []string `json:"extensions"`
}

// Capability is a named area of service, with tags saying what it covers
// and over which transports ("ulp:dcsa:track-and-trace",
// "ulp:transport:rest"). It describes; Services' intents are what a party
// accepts. SpecURL is informative and never fetched.
type Capability struct {
	Name    string   `json:"name"`
	Version string   `json:"version"`
	SpecURL string   `json:"spec_url,omitempty"`
	Tags    []string `json:"tags,omitempty"`
}

// Transports are a party's endpoints, one per binding (docs/DESIGN.md,
// section A.7).
type Transports struct {
	REST string `json:"rest"`
	A2A  string `json:"a2a,omitempty"`
	MCP  string `json:"mcp,omitempty"`
}

// Document is a validated profile with its keys parsed.
type Document struct {
	Profile Profile
	keys    map[string]sig.Verifier
}

// Key returns the verifier the profile publishes under kid.
func (d *Document) Key(kid string) (sig.Verifier, bool) {
	v, ok := d.keys[kid]
	return v, ok
}

// KIDs returns the key IDs the profile publishes, sorted.
func (d *Document) KIDs() []string {
	return slices.Sorted(maps.Keys(d.keys))
}

// Accepts reports whether any of the profile's services lists intent.
func (d *Document) Accepts(intent string) bool {
	for _, s := range d.Profile.Services {
		if slices.Contains(s.Intents, intent) {
			return true
		}
	}
	return false
}

// Capability returns the profile's capability called name.
func (d *Document) Capability(name string) (Capability, bool) {
	for _, c := range d.Profile.Capabilities {
		if c.Name == name {
			return c, true
		}
	}
	return Capability{}, false
}

// SupportsDialect reports whether the profile lists standard (a header
// dialect's standard, such as "DCSA_TNT") in supported_dialects.
func (d *Document) SupportsDialect(standard string) bool {
	return slices.Contains(d.Profile.SupportedDialects, standard)
}

// ParseOptions configures ParseProfile.
type ParseOptions struct {
	// Domain is the domain the profile was fetched from. The profile must
	// name it as its party, and its endpoints must be on it.
	Domain string
	// Algorithms are the key algorithms accepted. Nil means
	// sig.DefaultAlgorithms().
	Algorithms sig.Algorithms
	// AllowHTTP accepts http endpoints (insecure development mode only).
	AllowHTTP bool
}

// ParseProfile parses and validates a profile fetched from opts.Domain.
func ParseProfile(data []byte, opts ParseOptions) (*Document, error) {
	if len(data) > MaxProfileSize {
		return nil, fmt.Errorf("discovery: profile is %d bytes, over the %d limit", len(data), MaxProfileSize)
	}
	var p Profile
	if err := strictjson.UnmarshalKnown(data, &p); err != nil {
		return nil, fmt.Errorf("discovery: profile: %w", err)
	}
	algs := opts.Algorithms
	if algs == nil {
		algs = sig.DefaultAlgorithms()
	}
	d := &Document{Profile: p, keys: map[string]sig.Verifier{}}
	if err := d.validate(opts, algs); err != nil {
		return nil, fmt.Errorf("discovery: profile of %s: %w", logsafe.Quote(opts.Domain), err)
	}
	return d, nil
}

func (d *Document) validate(opts ParseOptions, algs sig.Algorithms) error {
	p := &d.Profile
	if p.ProtocolVersion != envelope.Version {
		return fmt.Errorf("protocolVersion %s is not %q", logsafe.Quote(p.ProtocolVersion), envelope.Version)
	}
	if !envelope.ValidDomain(opts.Domain) || p.Party.Domain != opts.Domain {
		return fmt.Errorf("party.domain %s is not the domain it was fetched from", logsafe.Quote(p.Party.Domain))
	}
	if p.Party.Name == "" || utf8.RuneCountInString(p.Party.Name) > 128 || strings.ContainsFunc(p.Party.Name, isControl) {
		return errors.New("party.name must be 1-128 characters without control characters")
	}
	if len(p.Party.Roles) == 0 {
		return errors.New("party.roles is empty")
	}
	for _, r := range p.Party.Roles {
		if r != RoleOperator && r != RoleShipper {
			return fmt.Errorf("party.roles: %s is not a role", logsafe.Quote(r))
		}
	}
	if len(p.Services) == 0 {
		return errors.New("services is empty")
	}
	for _, s := range p.Services {
		if s.Name == "" || len(s.Name) > 64 || strings.ContainsFunc(s.Name, isControl) {
			return errors.New("services: a name must be 1-64 characters without control characters")
		}
		for _, x := range s.Extensions {
			if x == "" || len(x) > 64 || strings.ContainsFunc(x, isControl) {
				return errors.New("services: an extension must be 1-64 characters without control characters")
			}
		}
		for _, in := range s.Intents {
			if !strings.HasPrefix(in, "ulp.") || len(in) > 64 {
				return fmt.Errorf("services: %s is not an intent name", logsafe.Quote(in))
			}
		}
	}
	if err := checkEndpoint("transports.rest", p.Transports.REST, opts); err != nil {
		return err
	}
	if p.Transports.A2A != "" {
		if err := checkEndpoint("transports.a2a", p.Transports.A2A, opts); err != nil {
			return err
		}
	}
	if p.Transports.MCP != "" {
		if err := checkEndpoint("transports.mcp", p.Transports.MCP, opts); err != nil {
			return err
		}
	}
	if err := checkCapabilities(p.Capabilities); err != nil {
		return err
	}
	if err := checkDialects(p.SupportedDialects); err != nil {
		return err
	}
	if len(p.Keys) == 0 || len(p.Keys) > MaxKeys {
		return fmt.Errorf("keys has %d entries, want 1-%d", len(p.Keys), MaxKeys)
	}
	for i, raw := range p.Keys {
		kid, v, err := algs.ParseJWK(raw)
		if err != nil {
			return fmt.Errorf("keys[%d]: %w", i, err)
		}
		if _, dup := d.keys[kid]; dup {
			return fmt.Errorf("keys: kid %s appears twice", logsafe.Quote(kid))
		}
		d.keys[kid] = v
	}
	return nil
}

var (
	// capabilityName is a reverse-DNS name, "dev.ulp.logistics.shipment".
	capabilityName = regexp.MustCompile(`^[a-z][a-z0-9-]*(\.[a-z0-9_-]+)+$`)
	// capabilityVersion is "0.0.1", "v2" or "2026-01-11".
	capabilityVersion = regexp.MustCompile(`^[A-Za-z0-9._-]{1,32}$`)
	// tagPattern is colon-separated lowercase words, "ulp:transport:rest".
	tagPattern = regexp.MustCompile(`^[a-z0-9]+(:[a-z0-9-]+)+$`)
	// dialectStandard is a header dialect's standard (pkg/envelope).
	dialectStandard = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,31}$`)
)

func checkCapabilities(cs []Capability) error {
	if len(cs) > MaxCapabilities {
		return fmt.Errorf("capabilities has %d entries, over %d", len(cs), MaxCapabilities)
	}
	names := map[string]bool{}
	for i, c := range cs {
		f := fmt.Sprintf("capabilities[%d]", i)
		if len(c.Name) > 128 || !capabilityName.MatchString(c.Name) {
			return fmt.Errorf("%s.name: %s is not a reverse-DNS name of at most 128 characters", f, logsafe.Quote(c.Name))
		}
		if names[c.Name] {
			return fmt.Errorf("%s.name: %s appears twice", f, c.Name)
		}
		names[c.Name] = true
		if !capabilityVersion.MatchString(c.Version) {
			return fmt.Errorf("%s.version: %s is not 1-32 of [A-Za-z0-9._-]", f, logsafe.Quote(c.Version))
		}
		if c.SpecURL != "" {
			u, err := url.Parse(c.SpecURL)
			if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Opaque != "" || len(c.SpecURL) > 256 ||
				strings.ContainsFunc(c.SpecURL, isControl) {
				return fmt.Errorf("%s.spec_url: %s is not an https URL of at most 256 characters", f, logsafe.Quote(c.SpecURL))
			}
		}
		if len(c.Tags) > MaxTags {
			return fmt.Errorf("%s.tags has %d entries, over %d", f, len(c.Tags), MaxTags)
		}
		tags := map[string]bool{}
		for _, t := range c.Tags {
			if len(t) > 64 || !tagPattern.MatchString(t) {
				return fmt.Errorf("%s.tags: %s is not a tag such as ulp:transport:rest", f, logsafe.Quote(t))
			}
			if tags[t] {
				return fmt.Errorf("%s.tags: %s appears twice", f, t)
			}
			tags[t] = true
		}
	}
	return nil
}

func checkDialects(ds []string) error {
	if len(ds) > MaxDialects {
		return fmt.Errorf("supported_dialects has %d entries, over %d", len(ds), MaxDialects)
	}
	seen := map[string]bool{}
	for _, d := range ds {
		if !dialectStandard.MatchString(d) {
			return fmt.Errorf("supported_dialects: %s is not 1-32 characters of [A-Z0-9_]", logsafe.Quote(d))
		}
		if seen[d] {
			return fmt.Errorf("supported_dialects: %s appears twice", d)
		}
		seen[d] = true
	}
	return nil
}

// checkEndpoint requires an absolute https URL (http in insecure mode) on
// the party's own domain, with nothing that hides where it points.
func checkEndpoint(field, s string, opts ParseOptions) error {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" || u.Opaque != "" || len(s) > 512 {
		return fmt.Errorf("%s: %s is not an absolute URL with a host and no userinfo, query or fragment", field, logsafe.Quote(s))
	}
	if u.Scheme != "https" && (!opts.AllowHTTP || u.Scheme != "http") {
		return fmt.Errorf("%s: %s is not https", field, logsafe.Quote(s))
	}
	if u.Hostname() != opts.Domain {
		return fmt.Errorf("%s: %s is not on %s", field, logsafe.Quote(s), opts.Domain)
	}
	// No explicit port: the domain's standard port is the endpoint, and a
	// local deployment's peer map says where that actually is.
	if u.Host != u.Hostname() {
		return fmt.Errorf("%s: %s names a port", field, logsafe.Quote(s))
	}
	return nil
}

// isControl matches characters that don't belong in a display name: control
// characters, line and paragraph separators, and bidirectional overrides.
func isControl(r rune) bool { return logsafe.Unsafe(r, false) }
