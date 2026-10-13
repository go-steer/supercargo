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

// Package config loads a node's per-company configuration
// (docs/DESIGN.md, section A.12). Loading is strict: an unknown or
// misspelled key is an error, because a security setting that is silently
// ignored is worse than one that fails to load.
package config

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"

	"github.com/go-steer/supercargo/internal/control"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/sig"
)

// TLS modes.
const (
	TLSLocalCA     = "local-ca"     // serve TLS with the given certificate; trust the given CA outbound
	TLSInsecureDev = "insecure-dev" // plain HTTP everywhere: nothing ties a profile to its domain
	// TLSProxy serves plain HTTP on the listener, behind a proxy that
	// serves https at the node's domain (Cloud Run, #38); outbound is https,
	// verified against the system roots (or caFile). Only ever behind such
	// a proxy: the listener itself has no TLS.
	TLSProxy = "proxy"
)

// Roles. A node may have several: a company can be a carrier and a
// shipper, or a shipper and a receiver (docs/DESIGN.md, A.6).
const (
	RoleOperator = "operator"
	RoleShipper  = "shipper"
	RoleReceiver = "receiver"
)

// Config is one company's node configuration.
type Config struct {
	Domain string `yaml:"domain"`
	Name   string `yaml:"name"`
	// Roles are the roles this node has, one or more.
	Roles []string `yaml:"roles"`
	// Role is the one-role form, still accepted: it becomes Roles.
	OneRole  string            `yaml:"role"`
	Listen   string            `yaml:"listen"`
	TLS      TLS               `yaml:"tls"`
	Keys     []Key             `yaml:"keys"`
	Peers    map[string]string `yaml:"peers"`
	Partners []string          `yaml:"partners"`

	Discovery Discovery  `yaml:"discovery"`
	Security  Security   `yaml:"security"`
	Operator  *Operator  `yaml:"operator"`
	Forwarder *Forwarder `yaml:"forwarder"` // operator and shipper
	Simulator *Simulator `yaml:"simulator"` // operator only
	Control   *Control   `yaml:"control"`
	Business  *Business  `yaml:"business"` // shipper only
	// AddressBook is the company's own list of the partners it knows
	// (internal/directory); Directory is somewhere to search for new ones.
	// Neither is trust: a partner's verified profile is.
	AddressBook *AddressBook `yaml:"addressBook"`
	Directory   *Directory   `yaml:"directory"`

	// Sections for later milestones, accepted so a full config loads.
	LLM    *LLM    `yaml:"llm"`
	Skills *Skills `yaml:"skills"`
	MCP    *MCP    `yaml:"mcp"`

	// Telemetry exports traces (docs/DESIGN.md, A.16). Absent: nothing is
	// exported unless the standard OTEL_EXPORTER_OTLP_* variables say so.
	Telemetry *Telemetry `yaml:"telemetry"`

	// Web serves the browser UI under /ui/ on the node's listener (#30).
	// Absent: no UI.
	Web *Web `yaml:"web"`

	// Google signs people in with the company's own Google OAuth client
	// (#39): the browser UI (web.auth: google) and the company's agents
	// over MCP (mcp.server.auth: google, as Gemini Enterprise calls).
	Google *Google `yaml:"google"`
}

// Sign-in modes.
const (
	AuthDemo   = "demo"   // web: personas as buttons, no password (loopback only)
	AuthGoogle = "google" // web and mcp: Google sign-in, personas by members
	AuthToken  = "token"  // mcp: one shared bearer token (the default)
)

// Google is the company's Google OAuth client ("Web application" type).
type Google struct {
	ClientID string `yaml:"clientID"`
	// ClientSecretEnv names the environment variable holding the client
	// secret, or ClientSecretFile is an absolute path to a file holding it,
	// readable by its owner only (one of the two).
	ClientSecretEnv  string `yaml:"clientSecretEnv"`
	ClientSecretFile string `yaml:"clientSecretFile"`
	// PublicURL is the node's origin as people and Google reach it
	// (https://host): the UI's redirect URI is PublicURL/ui/auth/callback,
	// and the MCP endpoint is PublicURL/mcp. https, unless the host is
	// localhost (trying it out on one machine).
	PublicURL string `yaml:"publicURL"`
}

// Web configures the browser UI.
type Web struct {
	// Auth is how people sign in. "demo" offers the personas as buttons,
	// with no password: for local deployments only (loopback, tls:
	// insecure-dev or local-ca). "google" signs people in with Google (the
	// google section) and makes each the first persona whose members
	// include their email.
	Auth string `yaml:"auth"`
	// Personas are who may sign in, and the roles each may act in.
	Personas []Persona `yaml:"personas"`
	// Hosts are extra host names the UI may be reached as, besides
	// localhost names and the listen address: a proxy's, such as a Cloud
	// Workstations port ("8441-<workstation host>"). Lowercase DNS names.
	Hosts []string `yaml:"hosts"`
	// FrameAncestors are the origins ("https://host" or "http://host:port")
	// that may show this UI in a frame: a presenter page on another node.
	// Without them, only this UI's own pages may frame it.
	FrameAncestors []string `yaml:"frameAncestors"`
	// Presenter, if set, serves /ui/presenter: the listed UIs side by side,
	// each in its own frame, each signed in by itself.
	Presenter *Presenter `yaml:"presenter"`
}

// Presenter lists the UIs the presenter page shows.
type Presenter struct {
	Panels []Panel `yaml:"panels"`
}

// Panel is one UI on the presenter page.
type Panel struct {
	Name string `yaml:"name"` // shown above it; 1-64 characters
	URL  string `yaml:"url"`  // the UI, an absolute http(s) URL ending in /ui/
}

// Persona is someone who signs in to the UI.
type Persona struct {
	ID    string   `yaml:"id"`    // 1-32 of [a-z0-9-]
	Name  string   `yaml:"name"`  // shown; 1-64 characters
	Title string   `yaml:"title"` // shown; at most 64 characters
	Roles []string `yaml:"roles"` // roles of this node this persona acts in
	// Members are who signs in as this persona with Google sign-in: an
	// exact email address, or "@domain" for every account of that
	// Workspace domain (Google must say so: an email alone isn't enough).
	Members []string `yaml:"members"`
}

// Telemetry configures OpenTelemetry export over OTLP.
type Telemetry struct {
	// Protocol is "http" (OTLP/HTTP, port 4318; the default) or "grpc"
	// (port 4317).
	Protocol string `yaml:"protocol"`
	// Endpoint is the collector: host:port (e.g. localhost:4318) or, for
	// http, a URL. Empty leaves it to OTEL_EXPORTER_OTLP_*.
	Endpoint string `yaml:"endpoint"`
	// MetricsEndpoint, if set, receives metrics instead of Endpoint (a
	// Prometheus with its OTLP receiver, say): host:port or, for http, a
	// URL (no path: /v1/metrics). One insecure setting covers both.
	MetricsEndpoint string `yaml:"metricsEndpoint"`
	// MetricInterval is how often metrics are exported (default 60s).
	MetricInterval time.Duration `yaml:"metricInterval"`
	// Insecure sends without TLS (a local collector).
	Insecure bool `yaml:"insecure"`
	// SampleRatio is the share of traces kept, 0 to 1 (default 1).
	SampleRatio *float64 `yaml:"sampleRatio"`
	// ServiceName names this node in traces (default: its domain).
	ServiceName string `yaml:"serviceName"`
	// TrustPeers are partners whose trace context this node continues
	// (both export to one backend, as in the demo). Everyone else's is
	// only linked to.
	TrustPeers []string `yaml:"trustPeers"`
}

// TLS configures transport security.
type TLS struct {
	Mode     string `yaml:"mode"`
	CertFile string `yaml:"certFile"` // the node's server certificate (local-ca)
	KeyFile  string `yaml:"keyFile"`
	CAFile   string `yaml:"caFile"` // trusted for partners' certificates; empty means the system roots
}

// Key is a signing key. Exactly one is supported until key rotation
// (docs/DESIGN.md, A.15). It comes from PrivateKeyFile, or from the
// environment variable PrivateKeyEnv names (a secret manager's, as on
// Cloud Run): one of the two.
type Key struct {
	KID            string `yaml:"kid"`
	PrivateKeyFile string `yaml:"privateKeyFile"`
	PrivateKeyEnv  string `yaml:"privateKeyEnv"`
}

// Discovery configures the key cache.
type Discovery struct {
	TTL          time.Duration `yaml:"ttl"`
	RefreshFloor time.Duration `yaml:"refreshFloor"`
}

// Security configures freshness and replay protection.
type Security struct {
	MaxSkew         time.Duration `yaml:"maxSkew"`
	ReplayRetention time.Duration `yaml:"replayRetention"`
}

// Operator configures an operator's booking service.
type Operator struct {
	RefPrefix       string        `yaml:"refPrefix"`
	ContainerPrefix string        `yaml:"containerPrefix"`
	Vessel          string        `yaml:"vessel"`
	Voyage          string        `yaml:"voyage"`
	DepartureHour   int           `yaml:"departureHour"`
	Transit         time.Duration `yaml:"transit"`
	Deny            []string      `yaml:"deny"`
	// Road makes this operator a road carrier too (or only one, without
	// the ocean fields above): it takes road bookings and amendments.
	Road *Road `yaml:"road"`
}

// Forwarder makes the node a freight forwarder: it takes door-to-door
// orders as their operator (and books their legs as the legs' shipper),
// so it needs the operator and shipper roles.
type Forwarder struct {
	// RefPrefix starts each order reference, e.g. "MF".
	RefPrefix string   `yaml:"refPrefix"`
	Deny      []string `yaml:"deny"` // domains whose orders are refused
	// Lanes are the journeys the forwarder runs: its routing guide. An
	// order is planned on the lane from its place of receipt to its place
	// of delivery.
	Lanes []Lane `yaml:"lanes"`
	// EDI are the carriers it books over EDIFACT file drops (a lane leg
	// with edi: true names one of them).
	EDI []EDIPartner `yaml:"edi"`
}

// EDIPartner is a carrier booked over EDIFACT: its EDI identity, the file
// drops (instructions out, acknowledgements and statuses in), the time
// zone agreed for EDI times, and how long its legs are planned to take
// (its acknowledgement gives no times).
type EDIPartner struct {
	Carrier string        `yaml:"carrier"` // the carrier's domain, as lanes name it
	ID      string        `yaml:"id"`      // its UNB identity
	Outbox  string        `yaml:"outbox"`  // where instructions go
	Inbox   string        `yaml:"inbox"`   // where its replies arrive
	Zone    string        `yaml:"zone"`    // e.g. Europe/Amsterdam (default UTC)
	Transit time.Duration `yaml:"transit"` // a leg's planned duration
}

// Lane is one journey, door to door: its legs, in order, each ending
// where the next starts.
type Lane struct {
	From string    `yaml:"from"` // UN/LOCODE of the place of receipt
	To   string    `yaml:"to"`   // UN/LOCODE of the place of delivery
	Legs []LaneLeg `yaml:"legs"`
}

// LaneLeg is one leg of a lane: who carries it, by what mode, to where.
type LaneLeg struct {
	Mode    string `yaml:"mode"`    // road or ocean
	Carrier string `yaml:"carrier"` // the carrier's domain
	Name    string `yaml:"name"`    // the carrier's name, as the customer is told it
	To      string `yaml:"to"`      // UN/LOCODE where the leg ends (the last leg's is the lane's To)
	// Facility is the terminal or depot at To where this leg hands over
	// to the next (not for the last leg: the order says where it ends).
	Facility string `yaml:"facility"`
	// EDI marks a road carrier without a ULP node, booked over EDIFACT.
	EDI bool `yaml:"edi"`
}

// LaneMaxLegs bounds a lane: a planned journey has at most 8 legs.
const LaneMaxLegs = 8

var (
	locode = regexp.MustCompile(`^[A-Z]{2}[A-Z2-9]{3}$`)
	// ediID is a UNB party identity, an..35.
	ediID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.:_-]{0,34}$`)
)

func (p *EDIPartner) validate(self string) error {
	switch {
	case !envelope.ValidDomain(p.Carrier) || p.Carrier == self:
		return fmt.Errorf("carrier %q is not another party's lowercase domain", p.Carrier)
	case !ediID.MatchString(p.ID):
		return fmt.Errorf("id %q is not an EDI identity (1-35 of [A-Za-z0-9.:_-])", p.ID)
	case p.Outbox == "" || p.Inbox == "" || filepath.Clean(p.Outbox) == filepath.Clean(p.Inbox):
		return errors.New("an outbox and an inbox, two different directories, are required")
	case p.Transit <= 0:
		return errors.New("a positive transit is required")
	}
	if p.Zone == "" {
		p.Zone = "UTC"
	}
	if _, err := time.LoadLocation(p.Zone); err != nil {
		return fmt.Errorf("zone %q: %w", p.Zone, err)
	}
	return nil
}

func (l *Lane) validate(self string) error {
	if !locode.MatchString(l.From) || !locode.MatchString(l.To) || l.From == l.To {
		return fmt.Errorf("from %q and to %q must be two different UN/LOCODEs", l.From, l.To)
	}
	if len(l.Legs) == 0 || len(l.Legs) > LaneMaxLegs {
		return fmt.Errorf("%d legs, want 1-%d", len(l.Legs), LaneMaxLegs)
	}
	from := l.From
	for i, leg := range l.Legs {
		at := fmt.Sprintf("legs[%d]", i)
		switch {
		case leg.Mode != "road" && leg.Mode != "ocean":
			return fmt.Errorf("%s: mode %q is not road or ocean", at, leg.Mode)
		case leg.EDI && leg.Mode != "road":
			return fmt.Errorf("%s: only a road leg is booked over EDI", at)
		case !envelope.ValidDomain(leg.Carrier) || leg.Carrier == self:
			return fmt.Errorf("%s: carrier %q is not another party's lowercase domain", at, leg.Carrier)
		case !textOK(leg.Name, 128):
			return fmt.Errorf("%s: name must be 1-128 characters of plain text", at)
		case !locode.MatchString(leg.To) || (leg.To == from && leg.Mode == "ocean"):
			return fmt.Errorf("%s: to %q must be a UN/LOCODE (another port, for an ocean leg)", at, leg.To)
		}
		last := i == len(l.Legs)-1
		if last && leg.To != l.To {
			return fmt.Errorf("%s: the last leg ends at %s, not the lane's %s", at, leg.To, l.To)
		}
		if !last && !textOK(leg.Facility, 128) {
			return fmt.Errorf("%s: a leg handing over needs the facility (1-128 characters of plain text) where it does", at)
		}
		if last && leg.Facility != "" {
			return fmt.Errorf("%s: the last leg ends where the order says; no facility", at)
		}
		from = leg.To
	}
	return nil
}

// textOK is a one-line display text of 1-max characters, plain (no
// control or bidi characters: unsafeRune).
func textOK(s string, max int) bool {
	return s != "" && utf8.RuneCountInString(s) <= max && utf8.ValidString(s) && !strings.ContainsFunc(s, unsafeRune)
}

// Ocean reports whether the operator takes ocean bookings: it has the
// ocean fields, or no road section (and the ocean fields are required).
func (o *Operator) Ocean() bool {
	return o.Road == nil || o.Vessel != "" || o.Voyage != "" || o.ContainerPrefix != "" || o.Transit != 0 || o.DepartureHour != 0
}

// Road configures a road carrier's in-memory booking system
// (operator.TruckBookings).
type Road struct {
	// Transit is how long a leg takes from pickup to delivery.
	Transit time.Duration `yaml:"transit"`
	// Lead is the least notice before a pickup (default 2h).
	Lead time.Duration `yaml:"lead"`
	// CTe issues a mock Brazilian CT-e for each leg.
	CTe *CTe `yaml:"cte"`
	// ECMRPrefix numbers an e-CMR for each leg, e.g. "ECMR-NL-2026".
	ECMRPrefix string `yaml:"ecmrPrefix"`
}

// CTe is the mock CT-e's issuer: its CNPJ (14 characters, bare; the first
// 12 may be letters) and its state's IBGE code (11-53; 35 is São Paulo).
type CTe struct {
	CNPJ  string `yaml:"cnpj"`
	State int    `yaml:"state"`
}

var (
	envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	// refPrefix makes references that must pass the bodies' 1-64 of
	// [A-Za-z0-9_-], with "-TD-" and a number to spare.
	refPrefix = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,39}$`)
	// voyageNumber goes into every eBL and event as voyage_number, which
	// the bodies check: 1-64 of [A-Za-z0-9_-], starting with a letter or
	// digit (intents' references).
	voyageNumber = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	modelName    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@/-]{0,127}$`)
	cnpj         = regexp.MustCompile(`^[0-9A-Z]{12}[0-9]{2}$`)
)

// LLM providers.
const (
	ProviderFake      = "fake"      // deterministic, no API key: tests and rehearsal
	ProviderAnthropic = "anthropic" // Claude: an API key, or Vertex AI
	ProviderGemini    = "gemini"    // Gemini: an API key, or Vertex AI
)

// LLM configures the model. Without it, an operator answers inquiries,
// and a shipper summarizes alerts, from a deterministic template.
//
// Credentials never go in the file. With backend "api" (the default) the
// key comes from the environment variable APIKeyEnv names. With backend
// "vertex" the model is reached through Google Cloud's Vertex AI with
// Application Default Credentials (a gcloud login, or a Cloud Run
// service's own identity): no key at all, only the project and location,
// from the environment variables ProjectEnv and LocationEnv name.
type LLM struct {
	Provider    string `yaml:"provider"` // fake, anthropic or gemini
	Backend     string `yaml:"backend"`  // api (default) or vertex
	Model       string `yaml:"model"`
	APIKeyEnv   string `yaml:"apiKeyEnv"`
	ProjectEnv  string `yaml:"projectEnv"`
	LocationEnv string `yaml:"locationEnv"`
	// ThinkingBudget (gemini only): the thinking tokens a reply may spend
	// on top of its own limit; 0 the default, negative no thinking
	// settings (a model without them).
	ThinkingBudget int `yaml:"thinkingBudget"`
}

// Model backends.
const (
	BackendAPI    = "api"
	BackendVertex = "vertex"
)

// Business points at a shipper's private business context file
// (internal/business): delivery deadlines and receiving slots its own model
// uses, never sent to partners.
type Business struct {
	File string `yaml:"file"`
}

// AddressBook points at the company's address book file.
type AddressBook struct {
	File string `yaml:"file"`
}

// Directory configures where to search for partners: a file for local
// runs and tests (Google Cloud API hub later, #79).
type Directory struct {
	File string `yaml:"file"`
}

// Skills configures company agent skills (milestone 4).
type Skills struct {
	Dir string `yaml:"dir"`
}

// MCP configures backend connectors (later).
type MCP struct {
	Connectors []struct {
		Name string `yaml:"name"`
		URL  string `yaml:"url"`
	} `yaml:"connectors"`
	// Server serves the shipper's assistant tools over MCP at /mcp, for the
	// company's own agents (internal/mcpserver).
	Server *MCPServer `yaml:"server"`
}

// MCPServer configures the node's MCP endpoint.
type MCPServer struct {
	// Auth is how callers sign in: "token" (the default), one shared token,
	// or "google", each person's own Google access token (the google
	// section's client), allowed if a persona acting as shipper has them
	// among its members.
	Auth string `yaml:"auth"`
	// With auth token: TokenEnv names the environment variable holding the bearer token
	// callers present (at least 32 bytes): never the token itself. Or
	// TokenFile, an absolute path to a file holding it, readable by its
	// owner only (one of the two).
	TokenEnv  string `yaml:"tokenEnv"`
	TokenFile string `yaml:"tokenFile"`
	// UIURL is the browser UI's address, for the link a draft's answer
	// carries (optional).
	UIURL string `yaml:"uiURL"`
}

// Simulator configures the operator's shipment simulator.
type Simulator struct {
	// HoursPerSecond is simulated hours per real second (default 1, at most
	// MaxHoursPerSecond).
	HoursPerSecond float64 `yaml:"hoursPerSecond"`
}

// MaxHoursPerSecond bounds the simulator's clock speed: a whole voyage in
// well under a second, for tests.
const MaxHoursPerSecond = 3600

// Control configures the node's local control API (internal/control): the
// shipper's CLI books through it, the presenter's ulpctl drives the
// operator's simulator.
type Control struct {
	// Socket is the Unix socket's absolute path, in a directory only the
	// node's user can enter (checked at startup).
	Socket string `yaml:"socket"`
}

// Load reads, decodes and validates the config at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the config path is the operator's own choice
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return Parse(data)
}

// Parse decodes and validates a config. Defaults are filled in.
func Parse(data []byte) (*Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	// A second YAML document would be silently ignored; refuse it.
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("config: only one YAML document is allowed")
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return &c, nil
}

// Has reports whether the node has role.
func (c *Config) Has(role string) bool { return slices.Contains(c.Roles, role) }

// Customer reports whether the node has a customer-side role: shipper or
// receiver. Both keep the same kind of records, bookings another party
// issued, apart from an operator role's.
func (c *Config) Customer() bool { return c.Has(RoleShipper) || c.Has(RoleReceiver) }

// normalizeRoles folds the one-role form into Roles and checks them.
func (c *Config) normalizeRoles() error {
	switch {
	case c.OneRole != "" && len(c.Roles) > 0:
		return errors.New("roles: give roles or role, not both")
	case c.OneRole != "":
		c.Roles, c.OneRole = []string{c.OneRole}, ""
	}
	if len(c.Roles) == 0 {
		return fmt.Errorf("roles: at least one of %s, %s and %s", RoleOperator, RoleShipper, RoleReceiver)
	}
	seen := map[string]bool{}
	for _, r := range c.Roles {
		if r != RoleOperator && r != RoleShipper && r != RoleReceiver {
			return fmt.Errorf("roles: %q is not %s, %s or %s", r, RoleOperator, RoleShipper, RoleReceiver)
		}
		if seen[r] {
			return fmt.Errorf("roles: %q appears twice", r)
		}
		seen[r] = true
	}
	return nil
}

func (c *Config) validate() error {
	if !envelope.ValidDomain(c.Domain) {
		return fmt.Errorf("domain %q is not a lowercase DNS domain", c.Domain)
	}
	if c.Name == "" || len(c.Name) > 128 || strings.ContainsFunc(c.Name, unsafeRune) {
		return errors.New("name must be 1-128 characters without control or format characters (it is published)")
	}
	if err := c.normalizeRoles(); err != nil {
		return err
	}
	if !validHostPort(c.Listen, true) {
		return fmt.Errorf("listen %q is not [host]:port", c.Listen)
	}
	switch c.TLS.Mode {
	case TLSLocalCA:
		if c.TLS.CertFile == "" || c.TLS.KeyFile == "" {
			return errors.New("tls: local-ca needs certFile and keyFile")
		}
	case TLSInsecureDev:
	case TLSProxy:
		if c.TLS.CertFile != "" || c.TLS.KeyFile != "" {
			return errors.New("tls: proxy takes no certFile or keyFile: the proxy in front serves https")
		}
	default:
		return fmt.Errorf("tls.mode %q is not %s, %s or %s", c.TLS.Mode, TLSLocalCA, TLSProxy, TLSInsecureDev)
	}
	if len(c.Keys) != 1 {
		return fmt.Errorf("keys: exactly one signing key is supported, got %d", len(c.Keys))
	}
	if k := c.Keys[0]; !sig.ValidKID(k.KID) || (k.PrivateKeyFile == "") == (k.PrivateKeyEnv == "") ||
		(k.PrivateKeyEnv != "" && !envName.MatchString(k.PrivateKeyEnv)) {
		return errors.New("keys: a key needs a valid kid, and a privateKeyFile or a privateKeyEnv (an environment variable name), one of the two")
	}
	for d, target := range c.Peers {
		if !envelope.ValidDomain(d) {
			return fmt.Errorf("peers: %q is not a lowercase domain", d)
		}
		if !validHostPort(target, false) {
			return fmt.Errorf("peers: %s → %q is not host:port", d, target)
		}
	}
	for _, d := range c.Partners {
		if !envelope.ValidDomain(d) || d == c.Domain {
			return fmt.Errorf("partners: %q is not a lowercase domain, or is this node itself", d)
		}
	}
	// Upper bounds too: a huge skew turns freshness checking off, and a huge
	// TTL keeps a withdrawn key verifying.
	if c.Discovery.TTL < 0 || c.Discovery.TTL > MaxTTL || c.Discovery.RefreshFloor < 0 || c.Discovery.RefreshFloor > MaxTTL {
		return fmt.Errorf("discovery: ttl and refreshFloor must be between 0 and %v", MaxTTL)
	}
	if c.Security.MaxSkew < 0 || c.Security.MaxSkew > MaxSkew || c.Security.ReplayRetention < 0 || c.Security.ReplayRetention > 24*time.Hour {
		return fmt.Errorf("security: maxSkew must be between 0 and %v, replayRetention between 0 and 24h", MaxSkew)
	}
	if c.Security.MaxSkew == 0 {
		c.Security.MaxSkew = 5 * time.Minute
	}
	if c.Security.ReplayRetention == 0 {
		c.Security.ReplayRetention = 3 * c.Security.MaxSkew // over twice the skew, with room
	}
	if c.Security.ReplayRetention <= 2*c.Security.MaxSkew {
		return fmt.Errorf("security: replayRetention %v must exceed twice maxSkew (%v)", c.Security.ReplayRetention, c.Security.MaxSkew)
	}
	if f := c.Forwarder; f != nil {
		if !c.Has(RoleOperator) || !c.Has(RoleShipper) {
			return errors.New("forwarder: a forwarder needs the operator role (toward its customers) and the shipper role (toward its carriers)")
		}
		if !refPrefix.MatchString(f.RefPrefix) {
			return fmt.Errorf("forwarder: refPrefix %q must be 1-40 of [A-Za-z0-9-], starting with a letter or digit", f.RefPrefix)
		}
		if o := c.Operator; o != nil && (o.RefPrefix == f.RefPrefix || o.RefPrefix+"-R" == f.RefPrefix) {
			// Truck legs on an ocean operator number under refPrefix-R.
			return errors.New("forwarder: refPrefix must differ from operator.refPrefix (and from it plus -R), or an order and a booking could share a reference")
		}
		for _, d := range f.Deny {
			if !envelope.ValidDomain(d) || d == c.Domain {
				return fmt.Errorf("forwarder: deny entry %q is not a lowercase domain", d)
			}
		}
		ediCarriers := map[string]bool{}
		for i, p := range f.EDI {
			if err := p.validate(c.Domain); err != nil {
				return fmt.Errorf("forwarder: edi[%d]: %w", i, err)
			}
			if ediCarriers[p.Carrier] {
				return fmt.Errorf("forwarder: edi[%d]: a second entry for %s", i, p.Carrier)
			}
			ediCarriers[p.Carrier] = true
		}
		seen := map[[2]string]bool{}
		for i, l := range f.Lanes {
			for j, leg := range l.Legs {
				if leg.EDI && !ediCarriers[leg.Carrier] {
					return fmt.Errorf("forwarder: lanes[%d].legs[%d]: an EDI leg with %s, which forwarder.edi doesn't list", i, j, leg.Carrier)
				}
			}
			if err := l.validate(c.Domain); err != nil {
				return fmt.Errorf("forwarder: lanes[%d]: %w", i, err)
			}
			if seen[[2]string{l.From, l.To}] {
				return fmt.Errorf("forwarder: lanes[%d]: a second lane %s → %s", i, l.From, l.To)
			}
			seen[[2]string{l.From, l.To}] = true
		}
	}
	switch {
	case c.Has(RoleOperator) && c.Operator == nil && c.Forwarder == nil:
		return errors.New("operator: the operator role needs an operator section (or a forwarder section)")
	case !c.Has(RoleOperator) && c.Operator != nil:
		return errors.New("operator: an operator section needs the operator role")
	case c.Operator != nil:
		o := c.Operator
		if !refPrefix.MatchString(o.RefPrefix) {
			return fmt.Errorf("operator: refPrefix %q must be 1-40 of [A-Za-z0-9-], starting with a letter or digit", o.RefPrefix)
		}
		if o.Ocean() {
			if len(o.ContainerPrefix) != 4 || o.Vessel == "" || o.Voyage == "" || o.Transit <= 0 || o.DepartureHour < 0 || o.DepartureHour > 23 {
				return errors.New("operator: a 4-letter containerPrefix, vessel, voyage, departureHour (0-23) and a positive transit are required (or, for a road carrier only, none of them and a road section)")
			}
			if !voyageNumber.MatchString(o.Voyage) {
				return fmt.Errorf("operator: voyage %q must be 1-64 of [A-Za-z0-9_-], starting with a letter or digit", o.Voyage)
			}
			for _, r := range o.ContainerPrefix {
				if r < 'A' || r > 'Z' {
					return fmt.Errorf("operator: containerPrefix %q must be 4 uppercase letters", o.ContainerPrefix)
				}
			}
		}
		if r := o.Road; r != nil {
			if r.Transit <= 0 || r.Lead < 0 {
				return errors.New("operator: road needs a positive transit, and a lead that isn't negative")
			}
			if r.Lead == 0 {
				r.Lead = 2 * time.Hour
			}
			if r.CTe != nil && (!cnpj.MatchString(r.CTe.CNPJ) || r.CTe.State < 11 || r.CTe.State > 53) {
				return fmt.Errorf("operator: road.cte needs a bare 14-character CNPJ and a state code 11-53 (got %q, %d)", r.CTe.CNPJ, r.CTe.State)
			}
			if r.ECMRPrefix != "" && !refPrefix.MatchString(r.ECMRPrefix) {
				return fmt.Errorf("operator: road.ecmrPrefix %q must be 1-40 of [A-Za-z0-9-], starting with a letter or digit", r.ECMRPrefix)
			}
		}
		for _, d := range o.Deny {
			if !envelope.ValidDomain(d) || d == c.Domain {
				return fmt.Errorf("operator: deny entry %q is not a lowercase domain", d)
			}
		}
	}
	if c.LLM != nil {
		if err := c.LLM.validate(); err != nil {
			return err
		}
	}
	if t := c.Telemetry; t != nil {
		if t.SampleRatio != nil && !(*t.SampleRatio >= 0 && *t.SampleRatio <= 1) {
			return fmt.Errorf("telemetry: sampleRatio %v is not between 0 and 1", *t.SampleRatio)
		}
		switch t.Protocol {
		case "":
			t.Protocol = "http"
		case "http", "grpc":
		default:
			return fmt.Errorf("telemetry: protocol %q is not http or grpc", t.Protocol)
		}
		for _, e := range []string{t.Endpoint, t.MetricsEndpoint} {
			if e == "" || validHostPort(e, false) {
				continue
			}
			u, err := url.Parse(e)
			if err != nil || t.Protocol != "http" || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
				u.User != nil || u.RawQuery != "" || u.Fragment != "" {
				return fmt.Errorf("telemetry: endpoint %q is not host:port (or, for http, an http(s) URL without credentials or query)", e)
			}
			// A URL's scheme decides TLS: it must agree with insecure.
			if (u.Scheme == "http") != t.Insecure {
				return fmt.Errorf("telemetry: endpoint %q and insecure: %v disagree (http needs insecure: true, https insecure: false)", e, t.Insecure)
			}
		}
		if t.MetricInterval < 0 {
			return fmt.Errorf("telemetry: metricInterval %v is negative", t.MetricInterval)
		}
		for _, d := range t.TrustPeers {
			if !envelope.ValidDomain(d) {
				return fmt.Errorf("telemetry: trustPeers entry %q is not a lowercase domain", d)
			}
		}
		if strings.ContainsFunc(t.ServiceName, unsafeRune) || len(t.ServiceName) > 128 {
			return fmt.Errorf("telemetry: serviceName %q is not a short plain name", t.ServiceName)
		}
	}
	if c.Business != nil && (!c.Customer() || !filepath.IsAbs(c.Business.File)) {
		return fmt.Errorf("business: for the shipper or receiver role only, with an absolute file path (got %q)", c.Business.File)
	}
	if c.AddressBook != nil && !filepath.IsAbs(c.AddressBook.File) {
		return fmt.Errorf("addressBook: file %q must be an absolute path", c.AddressBook.File)
	}
	if c.Directory != nil && !filepath.IsAbs(c.Directory.File) {
		return fmt.Errorf("directory: file %q must be an absolute path", c.Directory.File)
	}
	if c.MCP != nil && c.MCP.Server != nil {
		ms := c.MCP.Server
		if !c.Has(RoleShipper) {
			return errors.New("mcp: server: the MCP tools are the shipper role's; this node has no shipper role")
		}
		switch ms.Auth {
		case "":
			ms.Auth = AuthToken
		case AuthToken, AuthGoogle:
		default:
			return fmt.Errorf("mcp: server: auth %q is not %s or %s", ms.Auth, AuthToken, AuthGoogle)
		}
		switch {
		case ms.Auth == AuthGoogle && (ms.TokenEnv != "" || ms.TokenFile != ""):
			return errors.New("mcp: server: auth google takes no token: each caller presents their own")
		case ms.Auth == AuthGoogle && c.Google == nil:
			return errors.New("mcp: server: auth google needs the google section")
		case ms.Auth == AuthGoogle && (c.Web == nil || !slices.ContainsFunc(c.Web.Personas, func(p Persona) bool {
			return len(p.Members) > 0 && slices.Contains(p.Roles, RoleShipper)
		})):
			return errors.New("mcp: server: auth google allows the members of web personas acting as shipper, and there are none")
		case ms.Auth == AuthToken && (ms.TokenEnv == "") == (ms.TokenFile == ""):
			return errors.New("mcp: server: tokenEnv or tokenFile, one of the two")
		case ms.TokenEnv != "" && !envName.MatchString(ms.TokenEnv):
			return fmt.Errorf("mcp: server: tokenEnv %q is not an environment variable name", ms.TokenEnv)
		case ms.TokenFile != "" && !filepath.IsAbs(ms.TokenFile):
			return fmt.Errorf("mcp: server: tokenFile %q must be an absolute path", ms.TokenFile)
		}
		if ms.UIURL != "" {
			if u, err := url.Parse(ms.UIURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
				return fmt.Errorf("mcp: server: uiURL %q is not an http(s) URL", ms.UIURL)
			}
		}
	}
	if c.Skills != nil && (c.Skills.Dir == "" || !filepath.IsAbs(c.Skills.Dir)) {
		return fmt.Errorf("skills: dir %q must be an absolute path", c.Skills.Dir)
	}
	if !c.Has(RoleOperator) && c.Simulator != nil {
		return errors.New("simulator: for the operator role only")
	}
	if c.Has(RoleOperator) {
		if c.Simulator == nil {
			c.Simulator = &Simulator{}
		}
		// NaN fails both comparisons, so it is caught too.
		if !(c.Simulator.HoursPerSecond >= 0 && c.Simulator.HoursPerSecond <= MaxHoursPerSecond) {
			return fmt.Errorf("simulator: hoursPerSecond must be between 0 and %d", MaxHoursPerSecond)
		}
		if c.Simulator.HoursPerSecond == 0 {
			c.Simulator.HoursPerSecond = 1
		}
	}
	if err := c.validateGoogle(); err != nil {
		return err
	}
	if err := c.validateWeb(); err != nil {
		return err
	}
	if c.Control != nil {
		p := c.Control.Socket
		if !filepath.IsAbs(p) || filepath.Clean(p) != p || len(p) > control.MaxSocketPath {
			return fmt.Errorf("control: socket %q must be a clean absolute path of at most %d bytes", p, control.MaxSocketPath)
		}
	}
	return nil
}

// Bounds on durations that weaken checks as they grow.
const (
	MaxSkew = time.Hour
	MaxTTL  = 24 * time.Hour
)

// validHostPort accepts host:port with a numeric port. A listen address
// (listen) may have an empty host (all interfaces) and port 0 (any free
// port); a target to connect to needs both.
func validHostPort(s string, listen bool) bool {
	host, port, err := net.SplitHostPort(s)
	if err != nil || (host == "" && !listen) {
		return false
	}
	n, err := strconv.Atoi(port)
	minPort := 1
	if listen {
		minPort = 0
	}
	return err == nil && n >= minPort && n <= 65535
}

// unsafeRune matches characters that don't belong in a published name.
func (c *Config) validateWeb() error {
	w := c.Web
	if w == nil {
		return nil
	}
	switch w.Auth {
	case AuthDemo:
		// No password: only this machine may reach it. A hosted node signs
		// people in with Google instead.
		if !loopback(c.Listen) || (c.TLS.Mode != TLSInsecureDev && c.TLS.Mode != TLSLocalCA) {
			return fmt.Errorf("web: demo sign-in has no password, so the node must listen on loopback only (127.0.0.1, ::1 or localhost), not %q", c.Listen)
		}
	case AuthGoogle:
		if c.Google == nil {
			return errors.New("web: auth google needs the google section")
		}
	default:
		return fmt.Errorf("web: auth %q is not %s or %s", w.Auth, AuthDemo, AuthGoogle)
	}
	if len(w.Hosts) > 16 {
		return errors.New("web: at most 16 hosts")
	}
	for _, h := range w.Hosts {
		if !envelope.ValidDomain(h) {
			return fmt.Errorf("web: host %q is not a lowercase DNS name", h)
		}
	}
	if len(w.FrameAncestors) > 8 {
		return errors.New("web: at most 8 frameAncestors")
	}
	for _, o := range w.FrameAncestors {
		if !validOrigin(o) {
			return fmt.Errorf("web: frameAncestors %q is not an origin (http(s)://host[:port], nothing else)", o)
		}
	}
	if p := w.Presenter; p != nil {
		if len(p.Panels) == 0 || len(p.Panels) > 6 {
			return errors.New("web: presenter: 1-6 panels")
		}
		for _, panel := range p.Panels {
			u, err := url.Parse(panel.URL)
			if panel.Name == "" || utf8.RuneCountInString(panel.Name) > 64 || strings.ContainsFunc(panel.Name, unsafeRune) ||
				err != nil || !validOrigin(u.Scheme+"://"+u.Host) || u.Path != "/ui/" || u.RawQuery != "" || u.Fragment != "" || u.User != nil ||
				u.ForceQuery || panel.URL != u.Scheme+"://"+u.Host+"/ui/" {
				return fmt.Errorf("web: presenter panel %q needs a short name and a URL like https://host/ui/", panel.URL)
			}
		}
	}
	if len(w.Personas) == 0 || len(w.Personas) > 32 {
		return errors.New("web: 1-32 personas are required")
	}
	seen := map[string]bool{}
	for _, p := range w.Personas {
		if !personaID.MatchString(p.ID) || seen[p.ID] {
			return fmt.Errorf("web: persona id %q must be 1-32 of [a-z0-9-], unique", p.ID)
		}
		seen[p.ID] = true
		if p.Name == "" || utf8.RuneCountInString(p.Name) > 64 || utf8.RuneCountInString(p.Title) > 64 ||
			strings.ContainsFunc(p.Name+p.Title, unsafeRune) {
			return fmt.Errorf("web: persona %s needs a name of 1-64 characters and a title of at most 64, without control or format characters", p.ID)
		}
		if len(p.Roles) == 0 {
			return fmt.Errorf("web: persona %s acts in no role", p.ID)
		}
		for _, r := range p.Roles {
			if !c.Has(r) {
				return fmt.Errorf("web: persona %s acts as %q, which this node isn't", p.ID, r)
			}
		}
		if len(p.Members) > 256 {
			return fmt.Errorf("web: persona %s: at most 256 members", p.ID)
		}
		for _, m := range p.Members {
			if !validMember(m) {
				return fmt.Errorf("web: persona %s: member %q is not a lowercase email address or @domain", p.ID, m)
			}
		}
		if w.Auth == AuthGoogle && len(p.Members) == 0 {
			return fmt.Errorf("web: persona %s has no members, so nobody could sign in as it", p.ID)
		}
	}
	return nil
}

// validMember accepts "name@domain" or "@domain", lowercase.
func validMember(m string) bool {
	local, domain, ok := strings.Cut(m, "@")
	return ok && envelope.ValidDomain(domain) && strings.Count(domain, ".") > 0 &&
		len(local) <= 64 && !strings.ContainsFunc(local, func(r rune) bool {
		return r <= ' ' || r >= 0x7f || r == '@' || unicode.IsUpper(r)
	})
}

func (c *Config) validateGoogle() error {
	g := c.Google
	if g == nil {
		return nil
	}
	if !googleClientID.MatchString(g.ClientID) {
		return fmt.Errorf("google: clientID %q is not a Google OAuth client ID (...apps.googleusercontent.com)", g.ClientID)
	}
	switch {
	case (g.ClientSecretEnv == "") == (g.ClientSecretFile == ""):
		return errors.New("google: clientSecretEnv or clientSecretFile, one of the two")
	case g.ClientSecretEnv != "" && !envName.MatchString(g.ClientSecretEnv):
		return fmt.Errorf("google: clientSecretEnv %q is not an environment variable name", g.ClientSecretEnv)
	case g.ClientSecretFile != "" && !filepath.IsAbs(g.ClientSecretFile):
		return fmt.Errorf("google: clientSecretFile %q must be an absolute path", g.ClientSecretFile)
	}
	u, err := url.Parse(g.PublicURL)
	if err != nil || !validOrigin(g.PublicURL) || (u.Scheme != "https" && !localHost(u.Hostname())) {
		return fmt.Errorf("google: publicURL %q must be an https origin (https://host[:port]); plain http only for localhost", g.PublicURL)
	}
	if (c.Web == nil || c.Web.Auth != AuthGoogle) && (c.MCP == nil || c.MCP.Server == nil || c.MCP.Server.Auth != AuthGoogle) {
		return errors.New("google: unused: set web.auth or mcp.server.auth to google")
	}
	return nil
}

// localHost reports whether host names this machine.
func localHost(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

var googleClientID = regexp.MustCompile(`^[0-9]{1,32}-[a-z0-9]{1,64}\.apps\.googleusercontent\.com$`)

// llmEnvDefaults are the environment variables each provider reads by
// default: the names Claude Code and Google's Gen AI SDK use, so the
// usual setup scripts work as they are.
var llmEnvDefaults = map[string][3]string{ // provider: API key, Vertex project, Vertex location
	ProviderAnthropic: {"ANTHROPIC_API_KEY", "ANTHROPIC_VERTEX_PROJECT_ID", "CLOUD_ML_REGION"},
	ProviderGemini:    {"GEMINI_API_KEY", "GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_LOCATION"},
}

// ValidateForTest fills in l's defaults and checks it, as loading does.
func (l *LLM) ValidateForTest() error { return l.validate() }

func (l *LLM) validate() error {
	switch l.Provider {
	case ProviderFake:
		return nil // it needs nothing, and ignores the rest
	case ProviderAnthropic, ProviderGemini:
	default:
		return fmt.Errorf("llm: provider %q is not %s, %s or %s", l.Provider, ProviderFake, ProviderAnthropic, ProviderGemini)
	}
	d := llmEnvDefaults[l.Provider]
	switch l.Backend {
	case "", BackendAPI:
		l.Backend = BackendAPI
		if l.ProjectEnv != "" || l.LocationEnv != "" {
			return errors.New("llm: projectEnv and locationEnv are for backend: vertex")
		}
		if l.APIKeyEnv == "" {
			l.APIKeyEnv = d[0]
		}
	case BackendVertex:
		if l.APIKeyEnv != "" {
			return errors.New("llm: backend vertex takes no API key: it uses Application Default Credentials")
		}
		if l.ProjectEnv == "" {
			l.ProjectEnv = d[1]
		}
		if l.LocationEnv == "" {
			l.LocationEnv = d[2]
		}
	default:
		return fmt.Errorf("llm: backend %q is not %s or %s", l.Backend, BackendAPI, BackendVertex)
	}
	// The key itself never goes in the file: only the names of the
	// environment variables.
	for name, v := range map[string]string{"apiKeyEnv": l.APIKeyEnv, "projectEnv": l.ProjectEnv, "locationEnv": l.LocationEnv} {
		if v != "" && !envName.MatchString(v) {
			return fmt.Errorf("llm: %s %q is not an environment variable name", name, v)
		}
	}
	// The model ID ends up in the request's path.
	if l.Model != "" && (!modelName.MatchString(l.Model) || strings.Contains(l.Model, "..")) {
		return fmt.Errorf("llm: model %q is not a model ID", l.Model)
	}
	if l.ThinkingBudget != 0 && l.Provider != ProviderGemini {
		return errors.New("llm: thinkingBudget is for provider gemini")
	}
	if l.Provider == ProviderGemini && l.Model == "" {
		return errors.New("llm: gemini needs a model")
	}
	return nil
}

var personaID = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// validOrigin reports whether s is a bare http(s) origin: scheme, host and
// an optional port, nothing else (it goes into a Content Security Policy).
func validOrigin(s string) bool {
	u, err := url.Parse(s)
	// Exactly scheme://host[:port]: no trailing "?" or "#", no path.
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || s != u.Scheme+"://"+u.Host || u.ForceQuery {
		return false
	}
	if p := u.Port(); p != "" || strings.HasSuffix(u.Host, ":") {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 || p != strconv.Itoa(n) {
			return false
		}
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	return (envelope.ValidDomain(host) || host == "localhost" || (ip != nil && !ip.IsUnspecified())) && !strings.ContainsAny(s, " ;,'\"")
}

// loopback reports whether listen's host is this machine only.
func loopback(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func unsafeRune(r rune) bool {
	return unicode.In(r, unicode.Cc, unicode.Cf, unicode.Co, unicode.Cs) || r == 0x2028 || r == 0x2029
}

// Pinned are the domains the node works with by configuration, pinned in
// its key cache: partners, and every domain in the peer map.
func (c *Config) Pinned() []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range c.Partners {
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	for d := range c.Peers {
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return out
}

// LoadSigningKey reads the node's Ed25519 private key: a PKCS#8 PEM file
// readable by its owner only, in a directory only its owner can write to
// (anyone else who could write there could swap in their own key, which the
// node would then publish as its own).
//
// The checks run on the opened file, not the path, so the file checked is
// the file read.
func LoadSigningKey(path string) (ed25519.PrivateKey, error) {
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("config: signing key directory: %w", err)
	}
	if dir.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("config: signing key directory %s is writable by group or others (mode %v)", filepath.Dir(path), dir.Mode().Perm())
	}
	f, err := os.Open(path) //nolint:gosec // G304: the key path is the operator's own configuration
	if err != nil {
		return nil, fmt.Errorf("config: signing key: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("config: signing key: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("config: signing key %s is not a regular file", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("config: signing key %s is accessible by group or others (mode %v); chmod 600 it", path, info.Mode().Perm())
	}
	data, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return nil, fmt.Errorf("config: signing key: %w", err)
	}
	return ParseSigningKey(data, path)
}

// LoadSigningKeyEnv reads the node's Ed25519 private key, a PKCS#8 PEM
// block, from environment variable name, and then unsets the variable, so
// no process the node starts inherits it. (The process's initial
// environment, /proc/self/environ, still holds it: only its own user can
// read that.)
func LoadSigningKeyEnv(name string) (ed25519.PrivateKey, error) {
	data, ok := os.LookupEnv(name)
	if !ok || data == "" {
		return nil, fmt.Errorf("config: signing key: %s is not set", name)
	}
	_ = os.Unsetenv(name)
	return ParseSigningKey([]byte(data), "from "+name)
}

// ParseSigningKey parses a single PKCS#8 PEM "PRIVATE KEY" block holding
// an Ed25519 key; where names it in errors.
func ParseSigningKey(data []byte, where string) (ed25519.PrivateKey, error) {
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("config: signing key %s is not a single PEM \"PRIVATE KEY\" block", where)
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("config: signing key %s: %w", where, err)
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("config: signing key %s is %T, not Ed25519", where, k)
	}
	return priv, nil
}

// WriteSigningKey writes priv as a PKCS#8 PEM file readable by its owner
// only, synced to disk. It refuses to overwrite an existing file (or follow
// a symlink there), and removes a file it couldn't finish.
func WriteSigningKey(path string, priv ed25519.PrivateKey) error {
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: the operator's chosen path
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	err = pem.Encode(f, &pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("config: %w", err)
	}
	return nil
}
