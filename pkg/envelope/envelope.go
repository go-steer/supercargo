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

// Package envelope is the signed ULP message (docs/DESIGN.md, section A.4):
// a JWS (RFC 7515, flattened JSON serialization) whose payload holds a
// header naming the intent and the parties, an intent-specific JSON body,
// and attachments.
//
// The signature covers the payload bytes exactly as sent, and those same
// bytes are what Verify hands back, so nothing is re-serialized between
// checking a message and acting on it.
//
// Receiving is two steps, so that the caller resolves the key in between
// and unverified content cannot leak past it:
//
//	u, err := envelope.Parse(wire)              // strict parse, header validated
//	v, err := keys.Lookup(u.Sender(), u.KID())  // the caller's key resolution
//	env, err := u.Verify(v)                     // the only way to the Envelope
//
// This package checks what a single message can prove about itself. Replay,
// clock skew, the receiver and authorization are the node pipeline's.
package envelope

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"

	"github.com/go-steer/supercargo/pkg/internal/logsafe"
	"github.com/go-steer/supercargo/pkg/internal/strictjson"
	"github.com/go-steer/supercargo/pkg/sig"
)

const (
	// Version is the ulpVersion this package produces and accepts.
	Version = "0.2-demo"

	// Type is the JWS "typ" of an envelope. A distinct typ keeps any other
	// JWS, a delegation token in particular, from being taken for one.
	Type = "ulp-envelope+jws"

	// MaxSize is the largest serialized envelope Parse accepts, in bytes.
	// Parsing runs before authentication and costs far more than verifying,
	// so this stays near what the intents need (their bodies are a few KiB).
	// Transports must enforce it while reading (io.LimitReader,
	// http.MaxBytesReader): Parse only sees what was already buffered.
	MaxSize = 128 << 10
)

var (
	// ErrMalformed reports a message that is not well formed: bad JSON or
	// base64url, a missing, extra or mistyped member, or a header field that
	// breaks its rule.
	ErrMalformed = errors.New("envelope: malformed")

	// ErrBadSignature reports a signature that does not verify, or a key
	// whose algorithm is not the one the message names.
	ErrBadSignature = errors.New("envelope: bad signature")
)

// Envelope is the content of a ULP message: the JWS payload.
type Envelope struct {
	Header Header `json:"header"`
	// Body is the intent-specific JSON object. After Verify it is the
	// verified bytes, as sent. Decoding it per intent must match member
	// names exactly: encoding/json folds case, so for a signed
	// {"q": "a", "Q": "b"} it would hand a handler "b" where another
	// implementation reads "a".
	Body        json.RawMessage `json:"body"`
	Attachments Attachments     `json:"attachments"`
}

// Attachments travel in the payload, so they are signed with the body.
type Attachments struct {
	// Delegation is a delegation token (a JSON object), or nil when there is
	// none: Parse turns a JSON null into nil. Its own signature and claims
	// are the delegation package's to check.
	Delegation json.RawMessage `json:"delegation"`
}

// Member names, exactly as they appear on the wire.
var (
	jwsMembers         = []string{"protected", "payload", "signature"}
	protectedMembers   = []string{"alg", "kid", "typ"}
	payloadMembers     = []string{"header", "body", "attachments"}
	headerMembers      = []string{"ulpVersion", "intent", "messageId", "correlationId", "inReplyTo", "sentAt", "sender", "receiver", "bookingRef"}
	nullableHeader     = map[string]bool{"inReplyTo": true, "bookingRef": true}
	attachmentsMembers = []string{"delegation"}
)

// algPattern bounds the "alg" a message may name. Whether it is acceptable
// is decided by the key: it must equal the resolved Verifier's Alg.
var algPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,32}$`)

// Unverified is a parsed, well-formed message whose signature has not been
// checked. It exposes what key resolution needs and nothing a handler could
// act on; Verify is the only way to the Envelope.
type Unverified struct {
	alg, kid     string
	env          Envelope
	signingInput []byte
	signature    []byte
}

// Sender is the domain the message claims to come from: whose profile to
// resolve KID against.
func (u *Unverified) Sender() string { return u.env.Header.Sender }

// KID is the key ID from the protected header.
func (u *Unverified) KID() string { return u.kid }

// Alg is the algorithm the protected header names. The resolved key must
// be bound to the same one.
func (u *Unverified) Alg() string { return u.alg }

// ClaimedHeader returns the header as the sender claims it, validated for
// form but not authenticated. Use it to log a rejected message, never to
// decide anything.
func (u *Unverified) ClaimedHeader() Header {
	h := u.env.Header
	h.InReplyTo = clone(h.InReplyTo)
	h.BookingRef = clone(h.BookingRef)
	return h
}

func clone(s *string) *string {
	if s == nil {
		return nil
	}
	c := *s
	return &c
}

// Verify checks the signature with v, which must be the key the sender
// publishes under KID. It fails, wrapping ErrBadSignature, if v is bound to
// a different algorithm than the message names or the signature does not
// verify. On success it returns the Envelope.
//
// A nil error means only that this key signed these bytes. Freshness,
// replay, the receiver and authorization are still to be checked.
func (u *Unverified) Verify(v sig.Verifier) (*Envelope, error) {
	// Only Parse makes a usable Unverified; a zero or nil one verifies
	// nothing, whatever v would say.
	if u == nil || u.alg == "" || len(u.signature) == 0 {
		return nil, fmt.Errorf("%w: not a parsed message", ErrBadSignature)
	}
	if v == nil {
		return nil, fmt.Errorf("%w: no key", ErrBadSignature)
	}
	if v.Alg() != u.alg {
		return nil, fmt.Errorf("%w: message names alg %q, key is %q", ErrBadSignature, u.alg, v.Alg())
	}
	if err := v.Verify(u.signingInput, u.signature); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadSignature, err)
	}
	// A deep copy: the caller may modify its Envelope without touching u.
	return &Envelope{
		Header:      u.ClaimedHeader(), // verified now
		Body:        append(json.RawMessage(nil), u.env.Body...),
		Attachments: Attachments{Delegation: append(json.RawMessage(nil), u.env.Attachments.Delegation...)},
	}, nil
}

// Parse decodes a wire envelope and checks that it is well formed: strict
// JSON throughout (valid UTF-8, no duplicate keys, bounded nesting, nothing
// trailing), canonical unpadded base64url, exactly the defined members with
// the right JSON types, a protected header naming Type, a well-formed kid
// and an alg, and a payload that passes Envelope.Validate. Every error wraps
// ErrMalformed.
func Parse(data []byte) (*Unverified, error) {
	u, err := parse(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	return u, nil
}

func parse(data []byte) (*Unverified, error) {
	if len(data) > MaxSize {
		return nil, fmt.Errorf("%d bytes exceeds the %d byte limit", len(data), MaxSize)
	}
	jws, err := strictjson.DecodeObject(data)
	if err != nil {
		return nil, err
	}
	if err := exactMembers(jws, "JWS", jwsMembers); err != nil {
		return nil, err
	}
	enc := map[string]string{}
	for _, k := range jwsMembers {
		s, ok := jws[k].(string)
		if !ok {
			return nil, fmt.Errorf("JWS %s is not a string", k)
		}
		enc[k] = s
	}

	alg, kid, err := parseProtected(enc["protected"])
	if err != nil {
		return nil, err
	}
	payload, err := sig.DecodeBase64URL(enc["payload"])
	if err != nil {
		return nil, fmt.Errorf("JWS payload: %w", err)
	}
	if err := checkPayloadShape(payload); err != nil {
		return nil, err
	}
	// The strict decode and the shape check are what make this one safe:
	// encoding/json matches keys case-insensitively, lets the last duplicate
	// win and ignores unknown members, but by now the member names are known
	// to be exactly the expected ones.
	var env Envelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return nil, err
	}
	// The shape check already decoded the body and delegation strictly; on
	// unauthenticated input, decoding them again is only cost.
	if err := env.Header.validate(); err != nil {
		return nil, fmt.Errorf("header.%w", err)
	}
	if string(env.Attachments.Delegation) == "null" {
		env.Attachments.Delegation = nil // absent means nil, whatever the wire said
	}
	signature, err := sig.DecodeBase64URL(enc["signature"])
	if err != nil {
		return nil, fmt.Errorf("JWS signature: %w", err)
	}
	if len(signature) == 0 {
		return nil, errors.New("JWS signature is empty")
	}
	return &Unverified{
		alg:          alg,
		kid:          kid,
		env:          env,
		signingInput: []byte(enc["protected"] + "." + enc["payload"]),
		signature:    signature,
	}, nil
}

func parseProtected(b64 string) (alg, kid string, err error) {
	raw, err := sig.DecodeBase64URL(b64)
	if err != nil {
		return "", "", fmt.Errorf("JWS protected header: %w", err)
	}
	h, err := strictjson.DecodeObject(raw)
	if err != nil {
		return "", "", fmt.Errorf("JWS protected header: %w", err)
	}
	if err := exactMembers(h, "JWS protected header", protectedMembers); err != nil {
		return "", "", err
	}
	s := map[string]string{}
	for _, k := range protectedMembers {
		v, ok := h[k].(string)
		if !ok {
			return "", "", fmt.Errorf("JWS protected header %s is not a string", k)
		}
		s[k] = v
	}
	if s["typ"] != Type {
		return "", "", fmt.Errorf("JWS typ %s is not %q", logsafe.Quote(s["typ"]), Type)
	}
	if !sig.ValidKID(s["kid"]) {
		return "", "", fmt.Errorf("JWS kid %s is not 1-64 characters of [A-Za-z0-9._-]", logsafe.Quote(s["kid"]))
	}
	if !algPattern.MatchString(s["alg"]) {
		return "", "", fmt.Errorf("JWS alg %s is not an algorithm identifier", logsafe.Quote(s["alg"]))
	}
	return s["alg"], s["kid"], nil
}

// checkPayloadShape verifies the member names and JSON types of the
// payload.
//
// Only the payload's skeleton is decoded, into json.RawMessage: the body is
// checked by strictjson.Valid but never built into Go values, because this
// runs on unauthenticated input and a body is the bulk of it.
func checkPayloadShape(payload []byte) error {
	if err := strictjson.Valid(payload); err != nil {
		return fmt.Errorf("payload: %w", err)
	}
	top, err := rawObject(payload, "payload", payloadMembers)
	if err != nil {
		return err
	}
	header, err := rawObject(top["header"], "header", headerMembers)
	if err != nil {
		return err
	}
	for _, k := range headerMembers {
		switch kind(header[k]) {
		case '"':
		case 'n':
			if !nullableHeader[k] {
				return fmt.Errorf("header.%s is null", k)
			}
		default:
			return fmt.Errorf("header.%s is not a string", k)
		}
	}
	if kind(top["body"]) != '{' {
		return errors.New("body is not a JSON object")
	}
	att, err := rawObject(top["attachments"], "attachments", attachmentsMembers)
	if err != nil {
		return err
	}
	switch kind(att["delegation"]) {
	case 'n', '{':
		return nil
	default:
		return errors.New("attachments.delegation is neither null nor a JSON object")
	}
}

// rawObject decodes one level of a JSON object that strictjson.Valid has
// accepted (so its keys are unique) and checks its member names exactly.
func rawObject(data json.RawMessage, name string, members []string) (map[string]json.RawMessage, error) {
	if kind(data) != '{' {
		return nil, fmt.Errorf("%s is not a JSON object", name)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if err := exactMembers(m, name, members); err != nil {
		return nil, err
	}
	return m, nil
}

// kind is the first byte of a JSON value, which names its type: '{', '[',
// '"', 'n' (null), 't'/'f' (booleans), or a digit or '-'.
func kind(v json.RawMessage) byte {
	v = bytes.TrimLeft(v, " \t\r\n")
	if len(v) == 0 {
		return 0
	}
	return v[0]
}

// exactMembers asserts that m has exactly the given members.
func exactMembers[V any](m map[string]V, name string, members []string) error {
	for k := range m {
		if !slices.Contains(members, k) {
			return fmt.Errorf("%s has unknown member %s", name, logsafe.Quote(k))
		}
	}
	for _, k := range members {
		if _, ok := m[k]; !ok {
			return fmt.Errorf("%s has no %q member", name, k)
		}
	}
	return nil
}

// Validate checks the header (Header.Validate), that the body is a JSON
// object and that the delegation attachment is null or a JSON object, all
// decoded strictly. Seal calls it, and Parse applies it to every payload.
// Every error wraps ErrMalformed.
func (e *Envelope) Validate() error {
	if err := e.validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	return nil
}

func (e *Envelope) validate() error {
	if err := e.Header.validate(); err != nil {
		return fmt.Errorf("header.%w", err)
	}
	if len(e.Body) == 0 {
		return errors.New("body is empty")
	}
	if _, err := strictjson.DecodeObject(e.Body); err != nil {
		return fmt.Errorf("body: %w", err)
	}
	if len(e.Attachments.Delegation) == 0 {
		return nil
	}
	d, err := strictjson.Decode(e.Attachments.Delegation)
	if err != nil {
		return fmt.Errorf("attachments.delegation: %w", err)
	}
	switch d.(type) {
	case nil, map[string]any:
		return nil
	default:
		return errors.New("attachments.delegation is neither null nor a JSON object")
	}
}

// protected is the JWS protected header, in the member order Seal writes.
type protected struct {
	Alg string `json:"alg"`
	KID string `json:"kid"`
	Typ string `json:"typ"`
}

// wire is the flattened JWS JSON serialization.
type wire struct {
	Protected string `json:"protected"`
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

// Seal validates e and returns it as a signed wire envelope: a JWS signed by
// s, whose protected header names s's algorithm and kid. s must be the key
// the sender publishes under kid.
//
// The payload is e as encoding/json writes it, which compacts the body and
// escapes <, > and & in strings. What is signed, and so what the receiver's
// Verify returns, is that encoding: the same values, not necessarily the
// same bytes as e.Body.
func Seal(e *Envelope, kid string, s sig.Signer) ([]byte, error) {
	if s == nil {
		return nil, errors.New("envelope: no signer")
	}
	if err := e.Validate(); err != nil {
		return nil, err
	}
	if !sig.ValidKID(kid) {
		return nil, fmt.Errorf("%w: kid %s is not 1-64 characters of [A-Za-z0-9._-]", ErrMalformed, logsafe.Quote(kid))
	}
	if !algPattern.MatchString(s.Alg()) {
		return nil, fmt.Errorf("envelope: signer alg %s is not an algorithm identifier", logsafe.Quote(s.Alg()))
	}
	ph, err := json.Marshal(protected{Alg: s.Alg(), KID: kid, Typ: Type})
	if err != nil {
		return nil, fmt.Errorf("envelope: %w", err)
	}
	payload, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("envelope: %w", err)
	}
	w := wire{
		Protected: base64.RawURLEncoding.EncodeToString(ph),
		Payload:   base64.RawURLEncoding.EncodeToString(payload),
	}
	signature, err := s.Sign([]byte(w.Protected + "." + w.Payload))
	if err != nil {
		return nil, fmt.Errorf("envelope: signing: %w", err)
	}
	w.Signature = base64.RawURLEncoding.EncodeToString(signature)
	return json.Marshal(w)
}
