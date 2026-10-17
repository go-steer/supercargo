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

package delegation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/supercargo/pkg/sig"
)

const issuer = "vale-verde.example"

// golden is testdata/golden/delegation.json (dev/tools/gen-delegation-vectors).
type golden struct {
	Keys    []json.RawMessage `json:"keys"`
	Vectors []struct {
		Name   string `json:"name"`
		Wire   string `json:"wire"`
		Grant  *Grant `json:"grant"`
		Error  string `json:"error"`
		Reason string `json:"reason"`
	} `json:"vectors"`
}

// keyring is a KeyFunc over one domain's keys.
func keyring(domain string, keys map[string]sig.Verifier) KeyFunc {
	return func(_ context.Context, d, kid string) (sig.Verifier, error) {
		if v, ok := keys[kid]; ok && d == domain {
			return v, nil
		}
		return nil, fmt.Errorf("no key %s for %s", kid, d)
	}
}

func loadGolden(t *testing.T) (golden, KeyFunc) {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/golden/delegation.json")
	if err != nil {
		t.Fatal(err)
	}
	var g golden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	keys := map[string]sig.Verifier{}
	for _, k := range g.Keys {
		kid, v, err := sig.ParseJWK(k)
		if err != nil {
			t.Fatal(err)
		}
		keys[kid] = v
	}
	return g, keyring(issuer, keys)
}

// TestGolden: every vector joserfc made parses and verifies, or fails,
// exactly as it says, for the reason it names: a malformed token before
// any key is looked up, a bad signature only when verified with the key
// the issuer publishes under the token's kid.
func TestGolden(t *testing.T) {
	g, keys := loadGolden(t)
	if len(g.Vectors) < 40 {
		t.Fatalf("only %d vectors", len(g.Vectors))
	}
	ctx := context.Background()
	for _, v := range g.Vectors {
		u, err := Parse([]byte(v.Wire))
		if v.Error == "malformed" {
			if !errors.Is(err, ErrMalformed) || !strings.Contains(err.Error(), v.Reason) {
				t.Errorf("%s: %v, want malformed for %q", v.Name, err, v.Reason)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", v.Name, err)
			continue
		}
		got, err := u.Verify(ctx, keys)
		switch v.Error {
		case "signature":
			if !errors.Is(err, ErrBadSignature) || !strings.Contains(err.Error(), v.Reason) {
				t.Errorf("%s: %v, want a bad signature for %q", v.Name, err, v.Reason)
			}
		case "":
			if err != nil || !reflect.DeepEqual(got.g, *v.Grant) {
				t.Errorf("%s: %+v, %v; want %+v", v.Name, got, err, v.Grant)
			}
		default:
			t.Errorf("%s: unknown expectation %q", v.Name, v.Error)
		}
	}
}

type pair struct {
	s sig.Signer
	v sig.Verifier
}

func keyPair(t *testing.T) pair {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	s, _ := sig.NewEd25519Signer(priv)
	v, _ := sig.NewEd25519Verifier(pub)
	return pair{s, v}
}

var t0 = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func aGrant(t *testing.T) *Grant {
	t.Helper()
	g, err := New(issuer, "serra-transportes.example", "mercado-aurora.example", "SR-BKG-0913",
		[]string{ScopeSubscribe, ScopeRead}, t0.Add(400*time.Millisecond), 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func verified(t *testing.T, g *Grant) *Verified {
	t.Helper()
	k := keyPair(t)
	wire, err := Seal(g, "vv-1", k.s)
	if err != nil {
		t.Fatal(err)
	}
	u, err := Parse(wire)
	if err != nil {
		t.Fatal(err)
	}
	v, err := u.Verify(context.Background(), keyring(issuer, map[string]sig.Verifier{"vv-1": k.v}))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// TestSealRoundTrip: a sealed grant parses, verifies with the issuer's
// key, says what was granted and checks for its request; New orders the
// scopes, keeps whole seconds and gives each grant its own token ID.
func TestSealRoundTrip(t *testing.T) {
	g := aGrant(t)
	if !reflect.DeepEqual(g.Scope, []string{ScopeRead, ScopeSubscribe}) || g.IssuedAt != "2026-10-20T12:00:00Z" || g.ExpiresAt != "2026-11-19T12:00:00Z" ||
		!uuidPattern.MatchString(g.TokenID) || g.TokenID == aGrant(t).TokenID {
		t.Fatalf("%+v", g)
	}
	v := verified(t, g)
	if v.TokenID() != g.TokenID || v.Issuer() != issuer || v.Audience() != "serra-transportes.example" || v.Grantee() != "mercado-aurora.example" ||
		v.BookingRef() != "SR-BKG-0913" || !v.Expires().Equal(t0.Add(30*24*time.Hour)) || !reflect.DeepEqual(v.Scope(), g.Scope) || !v.Allows(ScopeRead) {
		t.Fatalf("%+v", v)
	}
	if err := v.Check(issuer, "serra-transportes.example", "mercado-aurora.example", "SR-BKG-0913", ScopeSubscribe, t0.Add(time.Hour), time.Minute); err != nil {
		t.Error(err)
	}
	// Scope is a copy.
	v.Scope()[0] = "x"
	if v.Scope()[0] != ScopeRead {
		t.Error("Scope shares the grant's")
	}
	s := Scopes()
	s[0] = "x"
	if Scopes()[0] != ScopeRead {
		t.Error("Scopes shares the package's")
	}
}

// TestCheck: every way a verified grant may not cover a request is denied:
// above all, a grant from anyone but the booking's shipper.
func TestCheck(t *testing.T) {
	both := verified(t, aGrant(t))
	g := aGrant(t)
	g.Scope = []string{ScopeRead}
	read := verified(t, g)
	const aud, grantee, ref = "serra-transportes.example", "mercado-aurora.example", "SR-BKG-0913"
	exp := t0.Add(30 * 24 * time.Hour)
	for name, tc := range map[string]struct {
		v                                   *Verified
		issuer, audience, grantee, ref, scp string
		at                                  time.Time
	}{
		"not the booking's shipper": {both, "evil.example", aud, grantee, ref, ScopeRead, t0.Add(time.Hour)},
		"another carrier":           {both, issuer, "other.example", grantee, ref, ScopeRead, t0.Add(time.Hour)},
		"another grantee":           {both, issuer, aud, "other.example", ref, ScopeRead, t0.Add(time.Hour)},
		"another booking":           {both, issuer, aud, grantee, "SR-BKG-0914", ScopeRead, t0.Add(time.Hour)},
		"a scope it lacks":          {read, issuer, aud, grantee, ref, ScopeSubscribe, t0.Add(time.Hour)},
		"an unknown scope":          {both, issuer, aud, grantee, ref, "booking:write", t0.Add(time.Hour)},
		"before it's issued":        {both, issuer, aud, grantee, ref, ScopeRead, t0.Add(-time.Minute - time.Second)},
		"expired":                   {both, issuer, aud, grantee, ref, ScopeRead, exp.Add(time.Minute)},
	} {
		if err := tc.v.Check(tc.issuer, tc.audience, tc.grantee, tc.ref, tc.scp, tc.at, time.Minute); !errors.Is(err, ErrDenied) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Within the skew, early or late, it holds: exactly at its edges too.
	for name, at := range map[string]time.Time{"early within skew": t0.Add(-time.Minute), "late within skew": exp.Add(time.Minute - time.Second)} {
		if err := both.Check(issuer, aud, grantee, ref, ScopeRead, at, time.Minute); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The log shows attacker text quoted.
	err := both.Check(issuer, aud, "a\nb.example", ref, ScopeRead, t0, time.Minute)
	if err == nil || strings.Contains(err.Error(), "\n") {
		t.Errorf("unquoted: %q", err)
	}
}

// TestVerifyRefusals: the key is the issuer's, looked up by the package:
// a grantee signing a token that names the shipper as issuer fails, and so
// do no lookup, a failing one, a nil key, another algorithm and no token.
func TestVerifyRefusals(t *testing.T) {
	ctx := context.Background()
	shipper, grantee := keyPair(t), keyPair(t)
	// The grantee forges a grant "from" the shipper, with its own key.
	wire, _ := Seal(aGrant(t), "vv-1", grantee.s)
	u, _ := Parse(wire)
	keys := map[string]KeyFunc{
		// The lookup the carrier does: the claimed issuer's published key.
		"forged by the grantee": keyring(issuer, map[string]sig.Verifier{"vv-1": shipper.v}),
		"a failing lookup":      func(context.Context, string, string) (sig.Verifier, error) { return nil, errors.New("down") },
		"a nil key":             func(context.Context, string, string) (sig.Verifier, error) { return nil, nil },
		"another algorithm":     func(context.Context, string, string) (sig.Verifier, error) { return wrongAlg{}, nil },
		"no lookup":             nil,
	}
	for name, k := range keys {
		if _, err := u.Verify(ctx, k); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: %v", name, err)
		}
	}
	var asked []string
	_, _ = u.Verify(ctx, func(_ context.Context, d, kid string) (sig.Verifier, error) {
		asked = append(asked, d+"/"+kid)
		return shipper.v, nil
	})
	if !reflect.DeepEqual(asked, []string{issuer + "/vv-1"}) {
		t.Errorf("asked for %v", asked)
	}
	if _, err := (&Unverified{}).Verify(ctx, keys["forged by the grantee"]); !errors.Is(err, ErrBadSignature) {
		t.Errorf("zero token: %v", err)
	}
	var nilU *Unverified
	if _, err := nilU.Verify(ctx, keys["forged by the grantee"]); !errors.Is(err, ErrBadSignature) {
		t.Errorf("nil token: %v", err)
	}
	if u.Issuer() != issuer || u.KID() != "vv-1" {
		t.Errorf("claimed %s %s", u.Issuer(), u.KID())
	}
}

type wrongAlg struct{}

func (wrongAlg) Alg() string              { return "ES256" }
func (wrongAlg) Verify(_, _ []byte) error { return nil }

// TestParseRefusals: what the golden vectors don't cover.
func TestParseRefusals(t *testing.T) {
	k := keyPair(t)
	wire, _ := Seal(aGrant(t), "vv-1", k.s)
	var w map[string]string
	_ = json.Unmarshal(wire, &w)
	with := func(key, v string) []byte {
		c := map[string]string{}
		for a, b := range w {
			c[a] = b
		}
		c[key] = v
		b, _ := json.Marshal(c)
		return b
	}
	for name, tc := range map[string]struct {
		data   []byte
		reason string
	}{
		"too big":          {[]byte(`{"x":"` + strings.Repeat("a", MaxSize) + `"}`), "limit"},
		"not JSON":         {[]byte(`{`), ""},
		"a number member":  {[]byte(`{"protected":1,"payload":"e30","signature":"AA"}`), "not a string"},
		"bad protected":    {with("protected", "!!"), "protected"},
		"protected array":  {with("protected", "W10"), "protected"},
		"protected number": {with("protected", "eyJhbGciOjEsImtpZCI6InZ2LTEiLCJ0eXAiOiJ1bHAtZGVsZWdhdGlvbitqd3MifQ"), "not a string"},
		"bad payload":      {with("payload", "!!"), "payload"},
		"payload array":    {with("payload", "W10"), "grant"},
		"empty signature":  {with("signature", ""), "empty"},
	} {
		if _, err := Parse(tc.data); !errors.Is(err, ErrMalformed) || !strings.Contains(err.Error(), tc.reason) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestNewAndSealRefusals: an unknown scope (a typo isn't a narrower
// grant), an invalid grant, a bad kid, no signer, a bad or "none" alg, a
// failing signer: nothing made.
func TestNewAndSealRefusals(t *testing.T) {
	k := keyPair(t)
	if _, err := New(issuer, "a.example", "b.example", "R-1", []string{"tnt:subcribe"}, t0, time.Hour); !errors.Is(err, ErrMalformed) {
		t.Errorf("a typo: %v", err)
	}
	if _, err := New(issuer, issuer, "b.example", "R-1", []string{ScopeRead}, t0, time.Hour); !errors.Is(err, ErrMalformed) {
		t.Errorf("issuer as audience: %v", err)
	}
	if _, err := New(issuer, "a.example", "b.example", "R-1", []string{ScopeRead}, t0, MaxTTL+time.Second); !errors.Is(err, ErrMalformed) {
		t.Errorf("too long: %v", err)
	}
	bad := aGrant(t)
	bad.Scope = nil
	if _, err := Seal(bad, "vv-1", k.s); !errors.Is(err, ErrMalformed) {
		t.Errorf("invalid grant: %v", err)
	}
	if _, err := Seal(aGrant(t), "bad kid", k.s); !errors.Is(err, ErrMalformed) {
		t.Errorf("bad kid: %v", err)
	}
	if _, err := Seal(aGrant(t), "vv-1", nil); err == nil {
		t.Error("no signer")
	}
	for _, alg := range []string{"a b", "none"} {
		if _, err := Seal(aGrant(t), "vv-1", badSigner{alg: alg}); err == nil {
			t.Errorf("alg %q", alg)
		}
	}
	if _, err := Seal(aGrant(t), "vv-1", badSigner{alg: "Ed25519", fail: true}); err == nil {
		t.Error("a signer that fails")
	}
}

type badSigner struct {
	alg  string
	fail bool
}

func (b badSigner) Alg() string { return b.alg }
func (b badSigner) Sign([]byte) ([]byte, error) {
	if b.fail {
		return nil, errors.New("hsm down")
	}
	return []byte{1}, nil
}

// TestGoSealedTokensForPython writes Go-sealed tokens for joserfc to check
// (gen-delegation-vectors verify-go), when SUPERCARGO_GO_SIGNED_OUT is set.
func TestGoSealedTokensForPython(t *testing.T) {
	out := os.Getenv("SUPERCARGO_GO_SIGNED_OUT")
	if out == "" {
		t.Skip("set SUPERCARGO_GO_SIGNED_OUT to write Go-sealed tokens for joserfc")
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	s, _ := sig.NewEd25519Signer(priv)
	jwk, _ := sig.Ed25519JWK(pub, "go-1")
	type token struct {
		Name  string `json:"name"`
		Wire  string `json:"wire"`
		Grant *Grant `json:"grant"`
	}
	doc := struct {
		JWK    json.RawMessage `json:"jwk"`
		Tokens []token         `json:"tokens"`
	}{JWK: jwk}
	read := aGrant(t)
	read.Scope = []string{ScopeRead}
	for name, g := range map[string]*Grant{"both_scopes": aGrant(t), "read_only": read} {
		wire, err := Seal(g, "go-1", s)
		if err != nil {
			t.Fatal(err)
		}
		doc.Tokens = append(doc.Tokens, token{name, string(wire), g})
	}
	raw, _ := json.MarshalIndent(doc, "", "  ")
	if err := os.WriteFile(out, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}
