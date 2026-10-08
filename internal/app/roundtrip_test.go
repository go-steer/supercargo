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

package app

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/config"
	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
	"github.com/go-steer/supercargo/pkg/sig"
)

// pair is a shipper and an operator built from config alone, over TLS with
// a shared test CA, joined by the peer map, each listing the other as a
// partner. Only app.New's wiring connects them.
type pair struct {
	shipper, operator *App
	shipperKey        string
}

func startPair(t *testing.T, operatorExtra string) *pair {
	t.Helper()
	return startPairWith(t, operatorExtra, "role: shipper")
}

// startPairWith is startPair with Cymbal Shoes' role line replaced by
// shipperRoles (more YAML may follow it).
func startPairWith(t *testing.T, operatorExtra, shipperRoles string) *pair {
	t.Helper()
	cert, key, ca := tlsFiles(t, "cymbal-shoes.example", "ocean-blue.example")
	sAddr, oAddr := freePort(t), freePort(t)
	tlsYAML := `tls: { mode: local-ca, certFile: "` + cert + `", keyFile: "` + key + `", caFile: "` + ca + `" }
peers: { cymbal-shoes.example: "` + sAddr + `", ocean-blue.example: "` + oAddr + `" }
`
	shipperKey := keyFile(t)
	sCfg, err := config.Parse([]byte(`domain: cymbal-shoes.example
name: Cymbal Shoes
` + shipperRoles + `
listen: "` + sAddr + `"
keys: [{ kid: cy-1, privateKeyFile: "` + shipperKey + `" }]
partners: [ocean-blue.example]
` + tlsYAML))
	if err != nil {
		t.Fatal(err)
	}
	oCfg, err := config.Parse([]byte(`domain: ocean-blue.example
name: Ocean Blue
role: operator
listen: "` + oAddr + `"
keys: [{ kid: ob-1, privateKeyFile: "` + keyFile(t) + `" }]
partners: [cymbal-shoes.example]
security: { maxSkew: 1m, replayRetention: 3m }
operator: { refPrefix: OB-BKG, containerPrefix: OCBU, vessel: Aurora, voyage: 42W, departureHour: 18, transit: 134h` + operatorExtra + ` }
` + tlsYAML))
	if err != nil {
		t.Fatal(err)
	}
	p := &pair{shipperKey: shipperKey}
	for _, c := range []**App{&p.shipper, &p.operator} {
		cfg := sCfg
		if c == &p.operator {
			cfg = oCfg
		}
		a, err := New(cfg, quiet())
		if err != nil {
			t.Fatal(err)
		}
		*c = a
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- a.Serve(ctx) }()
		t.Cleanup(func() { cancel(); <-done })
		waitListening(t, cfg.Listen)
	}
	return p
}

func request() *intents.BookingRequestBody {
	return &intents.BookingRequestBody{
		ShipperRef: "CY-PO-1", EarliestDeparture: "2026-10-06",
		ShipperName:     "Cymbal Shoes",
		PortOfLoading:   "NLRTM",
		PortOfDischarge: "USEWR",
		Equipment:       []intents.Equipment{{ISOType: "45G1", Count: 1, GrossWeightKg: 12000}},
		Cargo:           intents.Cargo{Description: "Shoes", HSCode: "6404"},
	}
}

// TestRoundTripFromConfig: booking works over TLS between two apps built
// from config, which takes the client-side CA, both peer maps and the
// fetcher's TLS policy all being wired; and partners are pinned.
func TestRoundTripFromConfig(t *testing.T) {
	p := startPair(t, "")
	conf, err := p.shipper.Shipper.Book(context.Background(), "ocean-blue.example", request())
	if err != nil {
		t.Fatalf("Book: %v", err)
	}
	if conf.Status != intents.StatusConfirmed {
		t.Errorf("confirmation %+v", conf)
	}
	if _, err := p.operator.Carrier.Bookings.Get("ocean-blue.example", conf.BookingRef); err != nil {
		t.Errorf("operator didn't store the booking: %v", err)
	}
	if !p.operator.Keys.IsPinned("cymbal-shoes.example") || !p.shipper.Keys.IsPinned("ocean-blue.example") {
		t.Error("partners aren't pinned in the key cache")
	}
	if p.operator.Server.TLSConfig.MinVersion != tls.VersionTLS12 {
		t.Error("server TLS minimum isn't 1.2")
	}
}

// TestDenyFromConfig: the operator's configured deny list reaches its
// booking service.
func TestDenyFromConfig(t *testing.T) {
	p := startPair(t, ", deny: [cymbal-shoes.example]")
	_, err := p.shipper.Shipper.Book(context.Background(), "ocean-blue.example", request())
	var re *node.RemoteError
	if !errors.As(err, &re) || re.Code != intents.CodeForbidden {
		t.Errorf("Book by a denied shipper = %v, want a forbidden RemoteError", err)
	}
}

// TestSkewFromConfig: the operator's configured maxSkew (1m) is the one in
// force. A request sent 2 minutes ago would pass the 5m default.
func TestSkewFromConfig(t *testing.T) {
	p := startPair(t, "")
	priv, err := config.LoadSigningKey(p.shipperKey)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := sig.NewEd25519Signer(priv)
	h := envelope.NewHeader(intents.BookingRequest, "cymbal-shoes.example", "ocean-blue.example", time.Now().Add(-2*time.Minute))
	b, _ := json.Marshal(request())
	msg, err := envelope.Seal(&envelope.Envelope{Header: h, Body: b}, "cy-1", signer)
	if err != nil {
		t.Fatal(err)
	}
	res := p.operator.Node.Process(context.Background(), msg)
	if res.Response == nil {
		t.Fatalf("no signed reply (status %d)", res.Status)
	}
	u, _ := envelope.Parse(res.Response)
	env := u.ClaimedHeader()
	if env.Intent != intents.Error {
		t.Fatalf("reply is %s, want ulp.error", env.Intent)
	}
	var reply map[string]any
	var w map[string]string
	_ = json.Unmarshal(res.Response, &w)
	payload, _ := sig.DecodeBase64URL(w["payload"])
	_ = json.Unmarshal(payload, &reply)
	if code := reply["body"].(map[string]any)["code"]; code != intents.CodeStale {
		t.Errorf("code %v, want stale: the configured skew isn't in force", code)
	}
}
