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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

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

// Roles.
const (
	RoleOperator = "operator"
	RoleShipper  = "shipper"
)

// Config is one company's node configuration.
type Config struct {
	Domain   string            `yaml:"domain"`
	Name     string            `yaml:"name"`
	Role     string            `yaml:"role"`
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

	// Sections for later milestones, accepted so a full config loads.
	LLM    *LLM    `yaml:"llm"`
	Skills *Skills `yaml:"skills"`
	MCP    *MCP    `yaml:"mcp"`
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

// LLM configures the model (milestone 4).
type LLM struct {
	Provider  string `yaml:"provider"`
	Model     string `yaml:"model"`
	APIKeyEnv string `yaml:"apiKeyEnv"`
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

func (c *Config) validate() error {
	if !envelope.ValidDomain(c.Domain) {
		return fmt.Errorf("domain %q is not a lowercase DNS domain", c.Domain)
	}
	if c.Name == "" || len(c.Name) > 128 || strings.ContainsFunc(c.Name, unsafeRune) {
		return errors.New("name must be 1-128 characters without control or format characters (it is published)")
	}
	if c.Role != RoleOperator && c.Role != RoleShipper {
		return fmt.Errorf("role %q is not %s or %s", c.Role, RoleOperator, RoleShipper)
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
	case c.Role == RoleOperator && c.Operator == nil:
		return errors.New("operator: an operator node needs an operator section")
	case c.Role == RoleShipper && c.Operator != nil:
		return errors.New("operator: a shipper node has no operator section")
	case c.Operator != nil:
		o := c.Operator
		if o.RefPrefix == "" || len(o.ContainerPrefix) != 4 || o.Vessel == "" || o.Voyage == "" || o.Transit <= 0 ||
			o.DepartureHour < 0 || o.DepartureHour > 23 {
			return errors.New("operator: refPrefix, a 4-letter containerPrefix, vessel, voyage, departureHour (0-23) and a positive transit are required")
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
	if c.Role != RoleOperator && c.Simulator != nil {
		return errors.New("simulator: for operator nodes only")
	}
	if c.Role == RoleOperator {
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
