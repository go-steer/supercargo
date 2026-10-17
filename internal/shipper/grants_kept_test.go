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

package shipper

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/delegation"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
	"github.com/go-steer/supercargo/pkg/sig"
)

// TestGrantsKept: a booking keeps its latest MaxGrants grants on record.
func TestGrantsKept(t *testing.T) {
	var last []byte
	s := grantShipper(t, &last, silent)
	var lastID string
	for range store.MaxGrants + 1 {
		g, err := s.Grant(context.Background(), carrier, "OB-BKG-1", "aurora.example", []string{delegation.ScopeRead}, time.Hour, "")
		if err != nil {
			t.Fatal(err)
		}
		lastID = g.TokenID
	}
	b, _ := s.Bookings.Get(carrier, "OB-BKG-1")
	if len(b.Grants) != store.MaxGrants || b.Grants[len(b.Grants)-1].TokenID != lastID {
		t.Errorf("%d kept", len(b.Grants))
	}
}

// TestSubscribeElsewhere: an answer about another booking isn't recorded.
func TestSubscribeElsewhere(t *testing.T) {
	var last []byte
	elsewhere := func(envelope.Header) (string, any) {
		return intents.TnTSubscriptionConfirmation, withRef{"OB-BKG-8", &intents.TnTSubscriptionConfirmationBody{Status: intents.StatusConfirmed,
			SubscriptionID: "SUB-D-1", ExpiresAt: "2030-01-01T00:00:00Z", Leg: &intents.PlannedLeg{Mode: intents.ModeRoad, From: "BRSAO", To: "BRGRU"}}}
	}
	s := delegatedShipper(t, &last, time.Now(), elsewhere, delegation.ScopeSubscribe)
	if _, err := s.Subscribe(context.Background(), carrier, "OB-BKG-7", "https://cymbal-shoes.example/ulp"); err == nil {
		t.Error("an answer about another booking was recorded")
	}
	if b, _ := s.Bookings.Get(carrier, "OB-BKG-7"); b.Delegated.SubscriptionID != "" {
		t.Errorf("recorded %+v", b.Delegated)
	}
}

// TestGrantToFollowOurselves: a grant whose carrier is this node is refused.
func TestGrantToFollowOurselves(t *testing.T) {
	now := time.Now()
	s := shipperWith(t, silent)
	g := &Grants{Shipper: s, From: map[string]bool{issuerDomain: true}}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := sig.NewEd25519Signer(priv)
	v, _ := sig.NewEd25519Verifier(pub)
	grant, _ := delegation.New(issuerDomain, "cymbal-shoes.example", "aurora.example", "R-1", []string{delegation.ScopeRead}, now, time.Hour)
	wire, _ := delegation.Seal(grant, "k-1", signer)
	u, _ := delegation.Parse(wire)
	ver, _ := u.Verify(context.Background(), func(context.Context, string, string) (sig.Verifier, error) { return v, nil })
	ref := "R-1"
	h := envelope.NewHeader(intents.DelegationGrant, issuerDomain, "cymbal-shoes.example", now)
	h.BookingRef = &ref
	// Addressed to us but granted to aurora: GrantedHere refuses it first.
	if err := g.Authorize(context.Background(), node.NewRequest(&envelope.Envelope{Header: h}, &intents.DelegationGrantBody{}, ver, now, time.Minute)); err == nil {
		t.Error("kept a grant to follow ourselves")
	}
}

// TestNoCancelDelegated: a booking followed by delegation isn't ours to
// cancel; nothing is sent.
func TestNoCancelDelegated(t *testing.T) {
	var last []byte
	s := delegatedShipper(t, &last, time.Now(), silent, delegation.ScopeRead)
	last = nil
	if _, err := s.Cancel(context.Background(), carrier, "OB-BKG-7"); err == nil || last != nil {
		t.Errorf("cancelled a delegated booking: %v, sent %d bytes", err, len(last))
	}
}
