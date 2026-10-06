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

	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/discovery"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
	"github.com/go-steer/supercargo/pkg/sig"
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

	// Keys resolves partners' keys; Replay records verified message IDs.
	Keys   KeyResolver
	Replay *store.Replay
	// MaxSkew is the allowed difference between sentAt and this node's
	// clock. Zero means 5m. Replay's retention must be at least twice it.
	MaxSkew time.Duration

	// Handlers serve the intents this node accepts. Each must be an intent
	// the intents package decodes.
	Handlers map[string]Handler
	// Poster sends outbound envelopes (Exchange). Nil means this node
	// only receives.
	Poster Poster
	// Observer receives every event. Nil discards them.
	Observer Observer
	// Now replaces time.Now (tests).
	Now func() time.Time
}

// Node is a configured ULP node. It is safe for concurrent use.
type Node struct {
	cfg     Config
	profile []byte
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
	if cfg.Observer == nil {
		cfg.Observer = func(Event) {}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	n := &Node{cfg: cfg}
	p, err := n.buildProfile()
	if err != nil {
		return nil, err
	}
	n.profile = p
	return n, nil
}

// Domain is the node's identity.
func (n *Node) Domain() string { return n.cfg.Domain }

// buildProfile renders the published profile and checks it with the same
// parser partners will use, so a misconfigured node fails at startup rather
// than in a partner's logs.
func (n *Node) buildProfile() ([]byte, error) {
	accepted := slices.Sorted(maps.Keys(n.cfg.Handlers))
	if accepted == nil {
		accepted = []string{}
	}
	p := discovery.Profile{
		ProtocolVersion: envelope.Version,
		Party:           discovery.Party{Domain: n.cfg.Domain, Name: n.cfg.Name, Roles: n.cfg.Roles},
		Services:        []discovery.Service{{Name: "ulp.freight", Intents: accepted, Extensions: []string{}}},
		Transports:      discovery.Transports{REST: n.cfg.RESTURL},
		Keys:            []json.RawMessage{n.cfg.PublicJWK},
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
// /.well-known/ulp, and the REST binding at /ulp.
func (n *Node) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+discovery.Path, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(n.profile)
	})
	mux.Handle("/ulp", rest.Handler(n))
	return mux
}
