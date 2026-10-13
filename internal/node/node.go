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

// Package node is a ULP node: one company's gateway for the protocol. It
// publishes the company's profile, runs every inbound envelope through the
// pipeline, and exchanges envelopes with partners (docs/DESIGN.md, section
// A.1).
//
// The pipeline is the security model, in order:
//
//	parse → addressed to this node → resolve the sender's key → verify the
//	signature → clock skew → replay → decode and validate the body →
//	authorize → handle → sign the reply
//
// Nothing reaches a handler, a store or a model before every step has
// passed, and no option skips one. Each step's outcome is an Event.
package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"time"

	"go.opentelemetry.io/otel/metric"

	"go.opentelemetry.io/otel/trace"

	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/discovery"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
	"github.com/go-steer/supercargo/pkg/sig"
	"github.com/go-steer/supercargo/pkg/transport/a2a"
	"github.com/go-steer/supercargo/pkg/transport/rest"
)

// KeyResolver finds the verifier a domain publishes under a kid.
// *discovery.Cache implements it.
type KeyResolver interface {
	Key(ctx context.Context, domain, kid string) (sig.Verifier, error)
}

// Poster sends a serialized envelope to a party (at the REST endpoint in its
// profile, or at an endpoint it registered) and returns its response.
// *rest.Client implements it.
type Poster interface {
	Post(ctx context.Context, receiver string, msg []byte) ([]byte, error)
	PostTo(ctx context.Context, receiver, endpoint string, msg []byte) ([]byte, error)
}

// A2APoster sends a serialized envelope over the A2A binding.
// *a2a.Client implements it, and returns a2a.ErrNoEndpoint for a receiver
// without an A2A endpoint.
type A2APoster interface {
	Post(ctx context.Context, receiver string, msg []byte) ([]byte, error)
}

// Config configures a Node.
type Config struct {
	// Domain is this company's identity.
	Domain string
	// Name and Roles are published in the profile.
	Name  string
	Roles []string
	// KID names Signer's key; PublicJWK is that key's public JWK, as
	// published in the profile.
	KID       string
	Signer    sig.Signer
	PublicJWK json.RawMessage
	// RESTURL is this node's ULP endpoint as published. Empty means
	// https://{Domain}/ulp.
	RESTURL string
	// AllowHTTP permits an http RESTURL (insecure development mode only).
	AllowHTTP bool
	// A2AURL is this node's A2A endpoint as published; empty means none.
	// A node with agentic skills (a handler for ulp.tnt.inquiry) serves
	// them there, and lists them in its agent card.
	A2AURL string

	// Keys resolves partners' keys; Replay records verified message IDs.
	Keys   KeyResolver
	Replay *store.Replay
	// MaxSkew is the allowed difference between sentAt and this node's
	// clock. Zero means 5m. Replay's retention must be at least twice it.
	MaxSkew time.Duration

	// Handlers serve the intents this node accepts. Each must be an intent
	// the intents package decodes.
	Handlers map[string]Handler
	// OutboundGuard, if set, sees every outbound body (requests and
	// replies) with its intent before it is sealed; an error refuses the
	// send. The shipper uses it to keep its private business context out
	// of the envelopes its own role sends.
	OutboundGuard func(intent string, body []byte) error
	// Poster sends outbound envelopes (Exchange). Nil means this node
	// only receives.
	Poster Poster
	// A2A sends outbound envelopes for the agentic intents (ExchangeA2A).
	// Nil sends them over Poster.
	A2A A2APoster
	// Observer receives every event. Nil discards them.
	Observer Observer
	// Tracer records the pipeline's spans. Nil uses the global
	// TracerProvider, which records nothing unless telemetry is set up.
	Tracer trace.Tracer
	// Meter records the pipeline's metrics. Nil uses the global
	// MeterProvider, which records nothing unless telemetry is set up.
	Meter metric.Meter
	// TrustTrace reports whether a sender's trace context (from the HTTP
	// headers, WithRemoteTrace) may be continued rather than only linked
	// to. Nil trusts nobody.
	TrustTrace func(domain string) bool
	// Now replaces time.Now (tests).
	Now func() time.Time
}

// Node is a configured ULP node. It is safe for concurrent use.
type Node struct {
	cfg      Config
	profile  []byte
	tracer   trace.Tracer
	duration metric.Float64Histogram
}

// New validates cfg and returns a Node.
func New(cfg Config) (*Node, error) {
	if !envelope.ValidDomain(cfg.Domain) {
		return nil, fmt.Errorf("node: domain %q is not a lowercase DNS domain", cfg.Domain)
	}
	if cfg.Signer == nil || cfg.Keys == nil || cfg.Replay == nil {
		return nil, errors.New("node: Signer, Keys and Replay are required")
	}
	if !sig.ValidKID(cfg.KID) {
		return nil, fmt.Errorf("node: kid %q is not valid", cfg.KID)
	}
	if cfg.MaxSkew <= 0 {
		cfg.MaxSkew = 5 * time.Minute
	}
	if cfg.Replay.Retention() <= 2*cfg.MaxSkew {
		return nil, fmt.Errorf("node: replay retention %v must exceed twice the clock skew %v", cfg.Replay.Retention(), cfg.MaxSkew)
	}
	for intent, h := range cfg.Handlers {
		if !intents.Supported(intent) || h == nil {
			return nil, fmt.Errorf("node: no body type for handled intent %q, or a nil handler", intent)
		}
	}
	if cfg.RESTURL == "" {
		cfg.RESTURL = "https://" + cfg.Domain + "/ulp"
	}
	if u, err := url.Parse(cfg.RESTURL); err != nil || u.Path != "/ulp" {
		return nil, fmt.Errorf("node: RESTURL %q must have the path /ulp, where Handler serves the REST binding", cfg.RESTURL)
	}
	if cfg.A2AURL != "" {
		if u, err := url.Parse(cfg.A2AURL); err != nil || u.Path != "/a2a" {
			return nil, fmt.Errorf("node: A2AURL %q must have the path /a2a, where Handler serves the A2A binding", cfg.A2AURL)
		}
	}
	if cfg.Observer == nil {
		cfg.Observer = func(Event) {}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	n := &Node{cfg: cfg, tracer: defaultTracer(cfg.Tracer), duration: newDuration(cfg.Meter)}
	p, err := n.buildProfile()
	if err != nil {
		return nil, err
	}
	n.profile = p
	return n, nil
}

// Domain is the node's identity.
func (n *Node) Domain() string { return n.cfg.Domain }

// capabilityTable names the areas of service a profile lists, after the
// ULP payload specification's discovery document: each is listed when the
// node accepts one of its intents. Its spec URL is the dialect schema of
// the first accepted intent that has one.
var capabilityTable = []struct {
	name    string
	intents []string
	tags    []string
}{
	{"dev.ulp.logistics.shipment", []string{intents.BookingRequest}, []string{"ulp:service", "ulp:dcsa:container-booking"}},
	{"dev.ulp.logistics.tracking", []string{intents.OceanTnTRequest, intents.TnTInquiry}, []string{"ulp:service", "ulp:dcsa:track-and-trace"}},
	{"dev.ulp.logistics.tracking.subscriber", []string{intents.TnTEvent, intents.TnTAlert}, []string{"ulp:subscriber", "ulp:dcsa:track-and-trace"}},
}

// capabilities are the capabilityTable entries for the accepted intents,
// tagged with the transports that carry them: REST always, A2A for the
// inquiry when the node serves A2A.
func capabilities(accepted []string, a2a bool) []discovery.Capability {
	var out []discovery.Capability
	for _, c := range capabilityTable {
		var has []string
		for _, in := range c.intents {
			if slices.Contains(accepted, in) {
				has = append(has, in)
			}
		}
		if has == nil {
			continue
		}
		cp := discovery.Capability{Name: c.name, Version: envelope.Version, Tags: append(slices.Clone(c.tags), "ulp:transport:rest")}
		if a2a && slices.Contains(has, intents.TnTInquiry) {
			cp.Tags = append(cp.Tags, "ulp:transport:a2a")
		}
		for _, in := range has {
			if d, ok := intents.DialectFor(in); ok {
				cp.SpecURL = d.SchemaURL
				break
			}
		}
		out = append(out, cp)
	}
	return out
}

// supportedDialects are the dialects a profile lists: the ocean ones, as
// every node has listed them, and the road dialect only when the node
// takes a road intent, so a node never claims a road capability it lacks.
func supportedDialects(accepted []string) []string {
	road := slices.ContainsFunc(accepted, func(i string) bool {
		d, ok := intents.DialectFor(i)
		return ok && d.Standard == intents.DialectULPRoad.Standard
	})
	out := []string{}
	for _, d := range intents.Dialects() {
		if d == intents.DialectULPRoad.Standard && !road {
			continue
		}
		out = append(out, d)
	}
	return out
}

// buildProfile renders the published profile and checks it with the same
// parser partners will use, so a misconfigured node fails at startup rather
// than in a partner's logs.
func (n *Node) buildProfile() ([]byte, error) {
	accepted := slices.Sorted(maps.Keys(n.cfg.Handlers))
	if accepted == nil {
		accepted = []string{}
	}
	p := discovery.Profile{
		ProtocolVersion:   envelope.Version,
		Party:             discovery.Party{Domain: n.cfg.Domain, Name: n.cfg.Name, Roles: n.cfg.Roles},
		Services:          []discovery.Service{{Name: "ulp.freight", Intents: accepted, Extensions: []string{}}},
		Capabilities:      capabilities(accepted, n.cfg.A2AURL != ""),
		SupportedDialects: supportedDialects(accepted),
		Transports:        discovery.Transports{REST: n.cfg.RESTURL, A2A: n.cfg.A2AURL},
		Keys:              []json.RawMessage{n.cfg.PublicJWK},
	}
	b, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("node: profile: %w", err)
	}
	doc, err := discovery.ParseProfile(b, discovery.ParseOptions{Domain: n.cfg.Domain, AllowHTTP: n.cfg.AllowHTTP})
	if err != nil {
		return nil, fmt.Errorf("node: own profile is invalid: %w", err)
	}
	v, ok := doc.Key(n.cfg.KID)
	if !ok || v.Alg() != n.cfg.Signer.Alg() {
		return nil, fmt.Errorf("node: PublicJWK does not publish kid %q for the signer's algorithm", n.cfg.KID)
	}
	// The published key must verify what the signer signs.
	probe := []byte("supercargo node key check")
	s, err := n.cfg.Signer.Sign(probe)
	if err != nil || v.Verify(probe, s) != nil {
		return nil, errors.New("node: PublicJWK is not the signer's public key")
	}
	return b, nil
}

// Handler serves the node's HTTP surface: its profile at
// /.well-known/ulp, the REST binding at /ulp, and, with an A2AURL, the A2A
// binding at /a2a and the agent card at /.well-known/agent-card.json.
func (n *Node) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+discovery.Path, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(n.profile)
	})
	mux.Handle("/ulp", rest.Handler(n))
	if n.cfg.A2AURL != "" {
		mux.Handle("/a2a", a2a.Handler(n))
		mux.Handle("GET "+a2a.WellKnownCardPath, a2a.CardHandler(n.Card()))
	}
	return mux
}

// agenticSkills are the A2A skills behind the intents that take one: a
// free-text task for this company's agent (docs/DESIGN.md, A.3).
var agenticSkills = map[string]func(name string) a2a.AgentSkill{
	intents.TnTInquiry: func(name string) a2a.AgentSkill {
		return a2a.AgentSkill{
			ID:   "shipment-inquiry",
			Name: "Shipment inquiry",
			Description: "Ask " + name + " a free-text question about one of your bookings. Send a signed ulp.tnt.inquiry " +
				"envelope in a data part; the answer, grounded in the booking's records and citing them, comes back as a " +
				"signed ulp.tnt.inquiry.response (or ulp.error) in a data part.",
			Tags:       []string{"ulp", "track-and-trace"},
			InputModes: []string{"application/json"}, OutputModes: []string{"application/json"},
		}
	},
}

// Card is the node's A2A agent card: its agentic skills only.
func (n *Node) Card() a2a.AgentCard {
	skills := []a2a.AgentSkill{}
	for _, intent := range slices.Sorted(maps.Keys(n.cfg.Handlers)) {
		if skill, ok := agenticSkills[intent]; ok {
			skills = append(skills, skill(n.cfg.Name))
		}
	}
	return a2a.AgentCard{
		Name: n.cfg.Name, Description: "The ULP agent of " + n.cfg.Name + " (" + n.cfg.Domain + ").",
		URL: n.cfg.A2AURL, ProtocolVersion: a2a.ProtocolVersion,
		PreferredTransport: a2a.TransportJSONRPC, DefaultInputModes: []string{"application/json"},
		DefaultOutputModes: []string{"application/json"}, Skills: skills,
	}
}
