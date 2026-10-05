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
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestRFC8032 checks the first two Ed25519 test vectors of RFC 8032
// section 7.1. Ed25519 is deterministic, so the signatures must match
// byte for byte.
func TestRFC8032(t *testing.T) {
	for _, tc := range []struct{ name, seed, pub, msg, sig string }{
		{"test 1",
			"9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60",
			"d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a",
			"",
			"e5564300c360ac729086e2cc806e828a84877f1eb8e5d974d873e065224901555fb8821590a33bacc61e39701cf9b46bd25bf5f0595bbe24655141438e7a100b"},
		{"test 2",
			"4ccd089b28ff96da9db6c346ec114e0f5b8a319f35aba624da8cf6ed4fb8a6fb",
			"3d4017c3e843895a92b70aa74d1b7ebc9c982ccf2ec4968cc0cd55f12af4660c",
			"72",
			"92a009a9f0d4cab8720e820b5f642540a2b27b5416503f8fb3762223ebdb69da085ac1e43e15996e458f3613d0f11d8c387b2eaeb4302aeeb00d291612bb0c00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			priv := ed25519.NewKeyFromSeed(mustHex(t, tc.seed))
			s, err := NewEd25519Signer(priv)
			if err != nil {
				t.Fatal(err)
			}
			if s.Alg() != Ed25519 {
				t.Errorf("signer Alg = %q", s.Alg())
			}
			got, err := s.Sign(mustHex(t, tc.msg))
			if err != nil {
				t.Fatal(err)
			}
			if hex.EncodeToString(got) != tc.sig {
				t.Errorf("signature = %x, want %s", got, tc.sig)
			}
			v, err := NewEd25519Verifier(mustHex(t, tc.pub))
			if err != nil {
				t.Fatal(err)
			}
			if err := v.Verify(mustHex(t, tc.msg), mustHex(t, tc.sig)); err != nil {
				t.Errorf("Verify: %v", err)
			}
		})
	}
}

// TestRFC8037 verifies the JWS example of RFC 8037 appendix A.4 with the
// JWK of appendix A.2 (given a kid and alg, which ULP requires).
func TestRFC8037(t *testing.T) {
	kid, v, err := ParseJWK([]byte(`{"kty":"OKP","crv":"Ed25519","x":"11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo","kid":"rfc8037","alg":"Ed25519"}`))
	if err != nil {
		t.Fatal(err)
	}
	if kid != "rfc8037" || v.Alg() != Ed25519 {
		t.Errorf("ParseJWK = %q, %q", kid, v.Alg())
	}
	input := "eyJhbGciOiJFZERTQSJ9.RXhhbXBsZSBvZiBFZDI1NTE5IHNpZ25pbmc"
	s, err := DecodeBase64URL("hgyY0il_MGCjP0JzlnLWG1PPOt7-09PGcvMg3AIbQR6dWbhijcNR4ki4iylGjg5BhVsPt9g7sVvpAr_MuM0KAg")
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Verify([]byte(input), s); err != nil {
		t.Errorf("Verify RFC 8037 example: %v", err)
	}
}

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func TestVerifyRejects(t *testing.T) {
	pub, priv := newKey(t)
	pub2, _ := newKey(t)
	s, _ := NewEd25519Signer(priv)
	v, _ := NewEd25519Verifier(pub)
	v2, _ := NewEd25519Verifier(pub2)
	msg := []byte("message")
	good, _ := s.Sign(msg)
	flipped := append([]byte(nil), good...)
	flipped[0] ^= 1

	if err := v.Verify(msg, good); err != nil {
		t.Fatalf("Verify good: %v", err)
	}
	for _, tc := range []struct {
		name string
		v    Verifier
		msg  []byte
		sig  []byte
	}{
		{"other message", v, []byte("message!"), good},
		{"other key", v2, msg, good},
		{"bit flip", v, msg, flipped},
		{"empty", v, msg, nil},
		{"truncated", v, msg, good[:63]},
		{"extended", v, msg, append(append([]byte(nil), good...), 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.v.Verify(tc.msg, tc.sig); !errors.Is(err, ErrBadSignature) {
				t.Errorf("Verify = %v, want ErrBadSignature", err)
			}
		})
	}
}

func TestConstructorsRejectBadKeys(t *testing.T) {
	if _, err := NewEd25519Signer(make([]byte, 32)); err == nil {
		t.Error("NewEd25519Signer took a 32-byte key")
	}
	if _, err := NewEd25519Signer(nil); err == nil {
		t.Error("NewEd25519Signer took a nil key")
	}
	if _, err := NewEd25519Verifier(make([]byte, 31)); err == nil {
		t.Error("NewEd25519Verifier took a 31-byte key")
	}
	if _, err := NewEd25519Verifier(nil); err == nil {
		t.Error("NewEd25519Verifier took a nil key")
	}
}

func TestJWKRoundTrip(t *testing.T) {
	pub, priv := newKey(t)
	j, err := Ed25519JWK(pub, "cy-2026-1")
	if err != nil {
		t.Fatal(err)
	}
	kid, v, err := ParseJWK(j)
	if err != nil {
		t.Fatalf("ParseJWK(%s): %v", j, err)
	}
	if kid != "cy-2026-1" || v.Alg() != Ed25519 {
		t.Errorf("ParseJWK = %q, %q", kid, v.Alg())
	}
	s, _ := NewEd25519Signer(priv)
	sig, _ := s.Sign([]byte("m"))
	if err := v.Verify([]byte("m"), sig); err != nil {
		t.Errorf("parsed key does not verify: %v", err)
	}
	if _, err := Ed25519JWK(pub, "bad kid"); err == nil {
		t.Error("Ed25519JWK took a bad kid")
	}
	if _, err := Ed25519JWK(pub[:31], "k"); err == nil {
		t.Error("Ed25519JWK took a short key")
	}
}

func TestParseJWKRejects(t *testing.T) {
	pub, _ := newKey(t)
	good, _ := Ed25519JWK(pub, "k1")
	edit := func(f func(m map[string]any)) string {
		var m map[string]any
		_ = json.Unmarshal(good, &m)
		f(m)
		b, _ := json.Marshal(m)
		return string(b)
	}
	x := edit(func(m map[string]any) {}) // sanity: unchanged parses
	if _, _, err := ParseJWK([]byte(x)); err != nil {
		t.Fatalf("ParseJWK(unchanged): %v", err)
	}
	xs := func(m map[string]any) string { return m["x"].(string) }

	for _, tc := range []struct {
		name string
		in   string
	}{
		{"not JSON", "jwk"},
		{"array", "[]"},
		{"duplicate member", strings.Replace(string(good), `"kid":"k1"`, `"kid":"k1","kid":"k2"`, 1)},
		{"private d", edit(func(m map[string]any) { m["d"] = "AAAA" })},
		{"private k", edit(func(m map[string]any) { m["k"] = "AAAA" })},
		{"no kid", edit(func(m map[string]any) { delete(m, "kid") })},
		{"kid not string", edit(func(m map[string]any) { m["kid"] = 1 })},
		{"kid with space", edit(func(m map[string]any) { m["kid"] = "k 1" })},
		{"no alg", edit(func(m map[string]any) { delete(m, "alg") })},
		{"alg not string", edit(func(m map[string]any) { m["alg"] = true })},
		{"alg EdDSA", edit(func(m map[string]any) { m["alg"] = "EdDSA" })},
		{"alg none", edit(func(m map[string]any) { m["alg"] = "none" })},
		{"alg HS256", edit(func(m map[string]any) { m["alg"] = "HS256" })},
		{"use enc", edit(func(m map[string]any) { m["use"] = "enc" })},
		{"use not string", edit(func(m map[string]any) { m["use"] = 1 })},
		{"no kty", edit(func(m map[string]any) { delete(m, "kty") })},
		{"kty EC", edit(func(m map[string]any) { m["kty"] = "EC" })},
		{"crv X25519", edit(func(m map[string]any) { m["crv"] = "X25519" })},
		{"no crv", edit(func(m map[string]any) { delete(m, "crv") })},
		{"no x", edit(func(m map[string]any) { delete(m, "x") })},
		{"x not string", edit(func(m map[string]any) { m["x"] = 1 })},
		{"x padded", edit(func(m map[string]any) { m["x"] = xs(m) + "=" })},
		{"x with newline", edit(func(m map[string]any) { m["x"] = xs(m)[:10] + "\n" + xs(m)[10:] })},
		{"x standard alphabet", edit(func(m map[string]any) { m["x"] = strings.NewReplacer("-", "+", "_", "/").Replace(xs(m)) + "+/" })},
		{"x short", edit(func(m map[string]any) { m["x"] = xs(m)[:40] })},
		{"x long", edit(func(m map[string]any) { m["x"] = xs(m) + "AAAA" })},
		{"unknown member", edit(func(m map[string]any) { m["x5u"] = "https://evil.example/cert" })},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.in == string(good) {
				t.Fatal("case did not change the JWK")
			}
			if _, v, err := ParseJWK([]byte(tc.in)); err == nil {
				t.Errorf("ParseJWK = %v, want an error", v)
			}
		})
	}
}

// fakeVerifier stands in for a future algorithm.
type fakeVerifier struct{ alg string }

func (f fakeVerifier) Alg() string { return f.alg }

func (fakeVerifier) Verify(_, _ []byte) error { return nil }

// TestAlgorithmsArePluggable shows the extension point: a new algorithm is
// one KeyParser entry, and only the algorithms in the set are accepted.
func TestAlgorithmsArePluggable(t *testing.T) {
	algs := DefaultAlgorithms()
	algs["ML-DSA-65"] = func(map[string]any) (Verifier, error) { return fakeVerifier{"ML-DSA-65"}, nil }
	kid, v, err := algs.ParseJWK([]byte(`{"kty":"AKP","alg":"ML-DSA-65","pub":"AAAA","kid":"pq-1"}`))
	if err != nil || kid != "pq-1" || v.Alg() != "ML-DSA-65" {
		t.Fatalf("custom algorithm: %q, %v, %v", kid, v, err)
	}
	if _, _, err := ParseJWK([]byte(`{"kty":"AKP","alg":"ML-DSA-65","pub":"AAAA","kid":"pq-1"}`)); err == nil {
		t.Error("the default set accepted an algorithm it does not define")
	}
	pub, _ := newKey(t)
	j, _ := Ed25519JWK(pub, "k1")
	if _, _, err := (Algorithms{}).ParseJWK(j); err == nil {
		t.Error("an empty set accepted an Ed25519 key")
	}
	// A parser must return a verifier for the algorithm it was asked for.
	liar := Algorithms{Ed25519: func(map[string]any) (Verifier, error) { return fakeVerifier{"none"}, nil }}
	if _, _, err := liar.ParseJWK(j); err == nil {
		t.Error("a verifier for the wrong algorithm was accepted")
	}
}

func TestDecodeBase64URL(t *testing.T) {
	for _, tc := range []struct {
		in string
		ok bool
	}{
		{"", true},
		{"AA", true},
		{"AAE", true},
		{"_-8", true},
		{"AB", false},   // nonzero trailing bits: not the canonical form of 0x00
		{"AA==", false}, // padding
		{"A", false},
		{"AA\n", false},
		{"A\nA", false},
		{"+/8", false}, // standard alphabet
		{"AA AA", false},
	} {
		_, err := DecodeBase64URL(tc.in)
		if tc.ok != (err == nil) {
			t.Errorf("DecodeBase64URL(%q) error = %v, want ok=%v", tc.in, err, tc.ok)
		}
	}
}

func TestValidKID(t *testing.T) {
	for kid, ok := range map[string]bool{
		"cy-2026-1": true, "A": true, "a.b_c-1": true, strings.Repeat("k", 64): true,
		"": false, strings.Repeat("k", 65): false, "-k": false, ".k": false, "k/1": false,
		"k 1": false, "../keys": false, "k\n": false, "é": false,
	} {
		if ValidKID(kid) != ok {
			t.Errorf("ValidKID(%q) = %v, want %v", kid, !ok, ok)
		}
	}
}
