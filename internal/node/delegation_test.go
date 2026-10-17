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

package node_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/pkg/delegation"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
)

// TestDelegationInPipeline: a third company asks about the shipper's
// booking with the shipper's grant attached, through the whole pipeline,
// and is answered; a token that isn't the issuer's, is malformed, or names
// an issuer whose key can't be found is refused before any handler.
func TestDelegationInPipeline(t *testing.T) {
	d := newDemo(t)
	ctx := context.Background()
	conf, err := d.ship.Book(ctx, operatorDomain, bookingRequest())
	if err != nil {
		t.Fatal(err)
	}
	ref := conf.BookingRef
	grantee := d.rival // a resolvable company with a key
	g, err := delegation.New(shipperDomain, operatorDomain, grantee.domain, ref, []string{delegation.ScopeRead}, time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ask := func(token []byte) error {
		h := envelope.NewHeader(intents.TnTInquiry, grantee.domain, operatorDomain, time.Now())
		h.BookingRef = &ref
		_, err := grantee.node.Exchange(node.WithDelegation(ctx, token), h, &intents.TnTInquiryBody{Question: "When does it arrive?"}, intents.TnTInquiryResponse)
		return err
	}
	good, err := d.shipper.node.SealDelegation(g)
	if err != nil {
		t.Fatal(err)
	}
	if err := ask(good); err != nil {
		t.Fatalf("the delegate's question: %v", err)
	}
	// Signed by the grantee under the shipper's kid: not the issuer's.
	forged, _ := delegation.Seal(g, d.shipper.kid, grantee.signer)
	// Mallory's domain resolves for nobody: its key can't be found.
	mg, _ := delegation.New("mallory.example", operatorDomain, grantee.domain, ref, []string{delegation.ScopeRead}, time.Now(), time.Hour)
	mallorys, _ := delegation.Seal(mg, d.mallory.kid, d.mallory.signer)
	for name, token := range map[string][]byte{"forged": forged, "malformed": []byte(`{"protected":"e30","payload":"e30","signature":"AA"}`), "unresolvable issuer": mallorys} {
		var re *node.RemoteError
		if err := ask(token); !errors.As(err, &re) || re.Code != intents.CodeForbidden {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A grant to sign for another issuer is refused here.
	if _, err := grantee.node.SealDelegation(g); err == nil {
		t.Error("sealed a grant issued by someone else")
	}
}

// TestRequestDelegation: without a delegation, a request can't show one.
func TestRequestDelegation(t *testing.T) {
	ref := "OB-BKG-1"
	h := envelope.NewHeader(intents.TnTInquiry, shipperDomain, operatorDomain, time.Now())
	h.BookingRef = &ref
	r := node.NewRequest(&envelope.Envelope{Header: h}, nil, nil, time.Now(), time.Minute)
	if err := r.Delegated(shipperDomain, delegation.ScopeRead); !errors.Is(err, node.ErrNoDelegation) {
		t.Errorf("Delegated: %v", err)
	}
	if err := r.GrantedHere(delegation.ScopeRead); !errors.Is(err, node.ErrNoDelegation) {
		t.Errorf("GrantedHere: %v", err)
	}
}
