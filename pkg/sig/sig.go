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

// Package sig is the pluggable signature layer under ULP's JWS envelopes
// (docs/DESIGN.md, section A.2).
//
// A Signer or Verifier is one key bound to exactly one JOSE algorithm. The
// envelope accepts a message only if its header names the algorithm of the
// key that verifies it, so the sender never picks how it is checked.
// Ed25519 (JOSE "Ed25519", RFC 9864) is the algorithm defined today. Adding
// one means implementing Signer and Verifier for it and registering a JWK
// parser in Algorithms; nothing in the envelope changes.
package sig

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/go-steer/supercargo/pkg/internal/logsafe"
	"github.com/go-steer/supercargo/pkg/internal/strictjson"
)

// Ed25519 is the JOSE algorithm identifier for Ed25519 signatures, the
// fully-specified name RFC 9864 registers in place of the polymorphic
// "EdDSA".
const Ed25519 = "Ed25519"

// ErrBadSignature reports a signature that is malformed or does not verify.
var ErrBadSignature = errors.New("sig: signature verification failed")

// Signer signs with one private key under one algorithm.
type Signer interface {
	// Alg is the JOSE algorithm identifier the signatures are made under.
	Alg() string
	// Sign returns the signature of msg.
	Sign(msg []byte) ([]byte, error)
}

// Verifier checks signatures for one public key under one algorithm.
type Verifier interface {
	// Alg is the JOSE algorithm identifier the key is bound to.
	Alg() string
	// Verify returns nil only if signature is a valid signature of msg.
	// Any other outcome wraps ErrBadSignature.
	Verify(msg, signature []byte) error
}

// kidPattern bounds key IDs. They end up in logs and in profile lookups, so
// they stay short and inert.
var kidPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidKID reports whether kid is a well-formed key ID: 1-64 characters of
// [A-Za-z0-9._-], starting with a letter or digit.
func ValidKID(kid string) bool { return kidPattern.MatchString(kid) }

// NewEd25519Signer returns a Signer for an Ed25519 private key. It keeps a
// copy of the key.
func NewEd25519Signer(priv ed25519.PrivateKey) (Signer, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("sig: Ed25519 private key is %d bytes, want %d", len(priv), ed25519.PrivateKeySize)
	}
	return ed25519Signer{append(ed25519.PrivateKey(nil), priv...)}, nil
}

type ed25519Signer struct{ priv ed25519.PrivateKey }

func (ed25519Signer) Alg() string { return Ed25519 }

func (s ed25519Signer) Sign(msg []byte) ([]byte, error) {
	return ed25519.Sign(s.priv, msg), nil
}

// NewEd25519Verifier returns a Verifier for an Ed25519 public key. It
// keeps a copy of the key, and rejects keys that are non-canonically
// encoded, off the curve or of small order: keys for which anyone could
// forge signatures.
func NewEd25519Verifier(pub ed25519.PublicKey) (Verifier, error) {
	if len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("sig: Ed25519 public key is %d bytes, want %d", len(pub), ed25519.PublicKeySize)
	}
	if err := checkEd25519PublicKey(pub); err != nil {
		return nil, err
	}
	return ed25519Verifier{append(ed25519.PublicKey(nil), pub...)}, nil
}

type ed25519Verifier struct{ pub ed25519.PublicKey }

func (ed25519Verifier) Alg() string { return Ed25519 }

func (v ed25519Verifier) Verify(msg, signature []byte) error {
	if len(signature) != ed25519.SignatureSize {
		return fmt.Errorf("%w: signature is %d bytes, want %d", ErrBadSignature, len(signature), ed25519.SignatureSize)
	}
	// Pure Ed25519 (no prehash, no context), as RFC 8037 specifies for JOSE.
	if !ed25519.Verify(v.pub, msg, signature) {
		return ErrBadSignature
	}
	return nil
}

// KeyParser builds a Verifier from the members of a public JWK whose "alg"
// it handles. "kid" and "alg" have already been checked.
type KeyParser func(jwk map[string]any) (Verifier, error)

// Algorithms maps JOSE algorithm identifiers to the parsers for their
// public JWKs: the set of algorithms a party accepts keys for. It is a
// value, not a global, so each caller decides what it accepts.
type Algorithms map[string]KeyParser

// DefaultAlgorithms returns the algorithms ULP defines: Ed25519.
func DefaultAlgorithms() Algorithms {
	return Algorithms{Ed25519: ParseEd25519JWK}
}

// ParseJWK parses a public JWK with DefaultAlgorithms.
func ParseJWK(data []byte) (kid string, v Verifier, err error) {
	return DefaultAlgorithms().ParseJWK(data)
}

// privateMembers are JWK members that carry private key material, across
// key types (RFC 7518 section 6, RFC 8037 section 2).
var privateMembers = []string{"d", "p", "q", "dp", "dq", "qi", "oth", "k"}

// ParseJWK parses a public JWK and returns its key ID and a Verifier bound
// to its algorithm. The JWK must name a "kid" (see ValidKID) and an "alg"
// in a; a "use" other than "sig", and any private key member, are
// rejected.
func (a Algorithms) ParseJWK(data []byte) (kid string, v Verifier, err error) {
	jwk, err := strictjson.DecodeObject(data)
	if err != nil {
		return "", nil, fmt.Errorf("sig: JWK: %w", err)
	}
	for _, m := range privateMembers {
		if _, ok := jwk[m]; ok {
			return "", nil, fmt.Errorf("sig: JWK has private key member %q", m)
		}
	}
	kid, err = stringMember(jwk, "kid")
	if err != nil {
		return "", nil, err
	}
	if !ValidKID(kid) {
		return "", nil, fmt.Errorf("sig: JWK kid %s is not 1-64 characters of [A-Za-z0-9._-]", logsafe.Quote(kid))
	}
	alg, err := stringMember(jwk, "alg")
	if err != nil {
		return "", nil, err
	}
	parse, ok := a[alg]
	if !ok {
		return "", nil, fmt.Errorf("sig: JWK alg %s is not supported", logsafe.Quote(alg))
	}
	if use, ok := jwk["use"]; ok && use != "sig" {
		return "", nil, errors.New("sig: JWK use is not \"sig\"")
	}
	v, err = parse(jwk)
	if err != nil {
		return "", nil, err
	}
	if v == nil {
		return "", nil, fmt.Errorf("sig: parser for %q returned no verifier", alg)
	}
	if v.Alg() != alg {
		return "", nil, fmt.Errorf("sig: parser for %q returned a %q verifier", alg, v.Alg())
	}
	return kid, v, nil
}

// ParseEd25519JWK is the KeyParser for Ed25519: an OKP key (RFC 8037) with
// crv "Ed25519" and the 32-byte public key in "x". Members beyond kty,
// crv, x, kid, alg and use are rejected.
func ParseEd25519JWK(jwk map[string]any) (Verifier, error) {
	for k := range jwk {
		switch k {
		case "kty", "crv", "x", "kid", "alg", "use":
		default:
			return nil, fmt.Errorf("sig: Ed25519 JWK has unexpected member %s", logsafe.Quote(k))
		}
	}
	if kty, _ := jwk["kty"].(string); kty != "OKP" {
		return nil, errors.New("sig: Ed25519 JWK kty is not \"OKP\"")
	}
	if crv, _ := jwk["crv"].(string); crv != "Ed25519" {
		return nil, errors.New("sig: Ed25519 JWK crv is not \"Ed25519\"")
	}
	x, err := stringMember(jwk, "x")
	if err != nil {
		return nil, err
	}
	pub, err := DecodeBase64URL(x)
	if err != nil {
		return nil, fmt.Errorf("sig: Ed25519 JWK x: %w", err)
	}
	return NewEd25519Verifier(pub)
}

// Ed25519JWK returns the public JWK that publishes pub under kid.
func Ed25519JWK(pub ed25519.PublicKey, kid string) ([]byte, error) {
	if len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("sig: Ed25519 public key is %d bytes, want %d", len(pub), ed25519.PublicKeySize)
	}
	if !ValidKID(kid) {
		return nil, fmt.Errorf("sig: kid %s is not 1-64 characters of [A-Za-z0-9._-]", logsafe.Quote(kid))
	}
	return json.Marshal(map[string]string{
		"kty": "OKP",
		"crv": "Ed25519",
		"x":   base64.RawURLEncoding.EncodeToString(pub),
		"kid": kid,
		"alg": Ed25519,
		"use": "sig",
	})
}

func stringMember(jwk map[string]any, name string) (string, error) {
	v, ok := jwk[name]
	if !ok {
		return "", fmt.Errorf("sig: JWK has no %q", name)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("sig: JWK %q is not a string", name)
	}
	return s, nil
}

// DecodeBase64URL decodes unpadded base64url (RFC 7515 section 2) and
// accepts only the canonical encoding, so each byte string has exactly one
// text form.
func DecodeBase64URL(s string) ([]byte, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	// The round trip is what makes it canonical: the decoder alone skips
	// '\r' and '\n' and, without Strict, ignores nonzero trailing bits.
	if err != nil || base64.RawURLEncoding.EncodeToString(b) != s {
		return nil, errors.New("not canonical unpadded base64url")
	}
	return b, nil
}
