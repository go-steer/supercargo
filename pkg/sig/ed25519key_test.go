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

package sig

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"math/big"
	"testing"
)

// le32 encodes n as 32 little-endian bytes, the Ed25519 point encoding.
func le32(n *big.Int) []byte {
	b := make([]byte, 32)
	n.FillBytes(b)
	return reverse(b)
}

func withSign(b []byte) []byte {
	b[31] |= 0x80
	return b
}

// TestWeakPublicKeysRejected covers the keys an attacker or a broken
// generator could publish to make a domain's signatures forgeable.
func TestWeakPublicKeysRejected(t *testing.T) {
	one := big.NewInt(1)
	pMinus1 := new(big.Int).Sub(edP, one)
	for _, tc := range []struct {
		name string
		pub  []byte
	}{
		{"identity (order 1)", le32(one)},
		{"y = -1 (order 2)", le32(pMinus1)},
		{"y = 0 (order 4)", le32(big.NewInt(0))},
		{"y = 0, other x (order 4)", withSign(le32(big.NewInt(0)))},
		{"identity with sign bit (non-canonical)", withSign(le32(one))},
		{"y = p (non-canonical 0)", le32(edP)},
		{"y = p + 1 (non-canonical identity)", le32(new(big.Int).Add(edP, one))},
		{"y = 2^255 - 1 (non-canonical)", le32(new(big.Int).Sub(new(big.Int).Lsh(one, 255), one))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewEd25519Verifier(tc.pub); err == nil {
				t.Errorf("NewEd25519Verifier(%x) succeeded, want an error", tc.pub)
			}
			jwk := `{"kty":"OKP","crv":"Ed25519","kid":"k","alg":"Ed25519","x":"` + base64.RawURLEncoding.EncodeToString(tc.pub) + `"}`
			if _, _, err := ParseJWK([]byte(jwk)); err == nil {
				t.Error("ParseJWK accepted the key")
			}
		})
	}
}

// TestIdentityKeyForgery documents why: with the identity point as public
// key, the signature R = identity, S = 0 verifies for any message under Go's
// crypto/ed25519. The check above is what stops that key at the door.
func TestIdentityKeyForgery(t *testing.T) {
	identity := le32(big.NewInt(1))
	forged := append(append([]byte(nil), identity...), make([]byte, 32)...)
	if !ed25519.Verify(identity, []byte("any message at all"), forged) {
		t.Skip("crypto/ed25519 no longer accepts this forgery; the key check is still defense in depth")
	}
	if _, err := NewEd25519Verifier(identity); err == nil {
		t.Fatal("the identity key was accepted, so this forgery would verify")
	}
}

func TestOffCurveKeyRejected(t *testing.T) {
	// Some y in [2, 100] has no x on the curve; find one with the same
	// arithmetic, then require the constructor to refuse it.
	for y := int64(2); y < 100; y++ {
		yy := big.NewInt(y)
		y2 := mulMod(yy, yy)
		u := new(big.Int).Sub(y2, big.NewInt(1))
		v := new(big.Int).Add(mulMod(edD, y2), big.NewInt(1))
		x2 := mulMod(u, new(big.Int).ModInverse(v, edP))
		if new(big.Int).ModSqrt(x2, edP) != nil {
			continue
		}
		if _, err := NewEd25519Verifier(le32(yy)); err == nil {
			t.Fatalf("y = %d is off the curve but was accepted", y)
		}
		return
	}
	t.Fatal("found no off-curve y below 100")
}

func TestGeneratedKeysAccepted(t *testing.T) {
	for range 20 {
		pub, _ := newKey(t)
		if _, err := NewEd25519Verifier(pub); err != nil {
			t.Fatalf("honest key %x rejected: %v", pub, err)
		}
	}
}

// TestSignatureMalleability: S + L encodes the same scalar as S. A verifier
// must reject it, or one signature has two valid forms.
func TestSignatureMalleability(t *testing.T) {
	pub, priv := newKey(t)
	s, _ := NewEd25519Signer(priv)
	v, _ := NewEd25519Verifier(pub)
	msg := []byte("m")
	good, _ := s.Sign(msg)
	order, _ := new(big.Int).SetString("7237005577332262213973186563042994240857116359379907606001950938285454250989", 10)
	sc := new(big.Int).SetBytes(reverse(append([]byte(nil), good[32:]...)))
	malleated := append(append([]byte(nil), good[:32]...), le32(sc.Add(sc, order))...)
	if err := v.Verify(msg, malleated); !errors.Is(err, ErrBadSignature) {
		t.Errorf("Verify(S+L) = %v, want ErrBadSignature", err)
	}
}

func TestKeysAreCopied(t *testing.T) {
	pub, priv := newKey(t)
	s, _ := NewEd25519Signer(priv)
	v, _ := NewEd25519Verifier(pub)
	sigBefore, _ := s.Sign([]byte("m"))
	for i := range priv {
		priv[i] = 0
	}
	for i := range pub {
		pub[i] = 0
	}
	sigAfter, _ := s.Sign([]byte("m"))
	if string(sigBefore) != string(sigAfter) {
		t.Error("zeroing the caller's private key changed the signer")
	}
	if err := v.Verify([]byte("m"), sigAfter); err != nil {
		t.Errorf("zeroing the caller's public key changed the verifier: %v", err)
	}
}

// TestParseJWKGenericChecks covers checks that ParseJWK makes for every
// algorithm, using a parser that would accept anything.
func TestParseJWKGenericChecks(t *testing.T) {
	permissive := Algorithms{"X": func(map[string]any) (Verifier, error) { return fakeVerifier{"X"}, nil }}
	if _, _, err := permissive.ParseJWK([]byte(`{"alg":"X","kid":"k"}`)); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	for _, m := range privateMembers {
		if _, _, err := permissive.ParseJWK([]byte(`{"alg":"X","kid":"k","` + m + `":"AAAA"}`)); err == nil {
			t.Errorf("private member %q accepted", m)
		}
	}
	nilParser := Algorithms{"X": func(map[string]any) (Verifier, error) { return nil, nil }}
	if _, _, err := nilParser.ParseJWK([]byte(`{"alg":"X","kid":"k"}`)); err == nil {
		t.Error("a parser returning no verifier was accepted")
	}
	failing := Algorithms{"X": func(map[string]any) (Verifier, error) { return nil, errors.New("bad key") }}
	if _, _, err := failing.ParseJWK([]byte(`{"alg":"X","kid":"k"}`)); err == nil {
		t.Error("a failing parser's error was dropped")
	}
}
