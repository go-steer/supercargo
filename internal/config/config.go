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
	Simulator *Simulator `yaml:"simulator"` // operator only
	Control   *Control   `yaml:"control"`
	Business  *Business  `yaml:"business"` // shipper only

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
}

// Web configures the browser UI.
type Web struct {
	// Auth is how people sign in. "demo" (the only mode so far) offers the
	// personas as buttons, with no password: for local deployments only
	// (tls: insecure-dev or local-ca). Hosted sign-in (IAP) comes with the
	// deployment (#81).
	Auth string `yaml:"auth"`
	// Personas are who may sign in, and the roles each may act in.
	Personas []Persona `yaml:"personas"`
	// Hosts are extra host names the UI may be reached as, besides
	// localhost names and the listen address: a proxy's, such as a Cloud
	// Workstations port ("8441-<workstation host>"). Lowercase DNS names.
	Hosts []string `yaml:"hosts"`
}

// Persona is someone who signs in to the UI.
type Persona struct {
	ID    string   `yaml:"id"`    // 1-32 of [a-z0-9-]
	Name  string   `yaml:"name"`  // shown; 1-64 characters
	Title string   `yaml:"title"` // shown; at most 64 characters
	Roles []string `yaml:"roles"` // roles of this node this persona acts in
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
// (docs/DESIGN.md, A.15).
type Key struct {
	KID            string `yaml:"kid"`
	PrivateKeyFile string `yaml:"privateKeyFile"`
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
}

var (
	envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	// refPrefix makes references that must pass the bodies' 1-64 of
	// [A-Za-z0-9_-], with "-TD-" and a number to spare.
	refPrefix = regexp.MustCompile(`^[A-Za-z0-9-]{1,40}$`)
	modelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@/-]{0,127}$`)
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
	default:
		return fmt.Errorf("tls.mode %q is not %s or %s", c.TLS.Mode, TLSLocalCA, TLSInsecureDev)
	}
	if len(c.Keys) != 1 {
		return fmt.Errorf("keys: exactly one signing key is supported, got %d", len(c.Keys))
	}
	if !sig.ValidKID(c.Keys[0].KID) || c.Keys[0].PrivateKeyFile == "" {
		return errors.New("keys: a key needs a valid kid and a privateKeyFile")
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
	switch {
	case c.Has(RoleOperator) && c.Operator == nil:
		return errors.New("operator: the operator role needs an operator section")
	case !c.Has(RoleOperator) && c.Operator != nil:
		return errors.New("operator: an operator section needs the operator role")
	case c.Operator != nil:
		o := c.Operator
		if o.RefPrefix == "" || len(o.ContainerPrefix) != 4 || o.Vessel == "" || o.Voyage == "" || o.Transit <= 0 ||
			o.DepartureHour < 0 || o.DepartureHour > 23 {
			return errors.New("operator: refPrefix, a 4-letter containerPrefix, vessel, voyage, departureHour (0-23) and a positive transit are required")
		}
		if !refPrefix.MatchString(o.RefPrefix) {
			return fmt.Errorf("operator: refPrefix %q must be 1-40 of [A-Za-z0-9-]", o.RefPrefix)
		}
		for _, r := range o.ContainerPrefix {
			if r < 'A' || r > 'Z' {
				return fmt.Errorf("operator: containerPrefix %q must be 4 uppercase letters", o.ContainerPrefix)
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
	if w.Auth != "demo" {
		return fmt.Errorf("web: auth %q is not demo (hosted sign-in comes with the deployment)", w.Auth)
	}
	// No password: only this machine may reach it. The hosted deployment
	// signs people in through IAP instead (#81).
	if !loopback(c.Listen) || (c.TLS.Mode != TLSInsecureDev && c.TLS.Mode != TLSLocalCA) {
		return fmt.Errorf("web: demo sign-in has no password, so the node must listen on loopback only (127.0.0.1, ::1 or localhost), not %q", c.Listen)
	}
	if len(w.Hosts) > 16 {
		return errors.New("web: at most 16 hosts")
	}
	for _, h := range w.Hosts {
		if !envelope.ValidDomain(h) {
			return fmt.Errorf("web: host %q is not a lowercase DNS name", h)
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
	}
	return nil
}

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
	if l.Provider == ProviderGemini && l.Model == "" {
		return errors.New("llm: gemini needs a model")
	}
	return nil
}

var personaID = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

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
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("config: signing key %s is not a single PEM \"PRIVATE KEY\" block", path)
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("config: signing key %s: %w", path, err)
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("config: signing key %s is %T, not Ed25519", path, k)
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
