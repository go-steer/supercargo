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

// Package delegation is ULP's delegation token (docs/DESIGN.md, A.4
// "Delegation"; milestone 7): a booking's shipper grants another company
// access to one booking with one carrier, for a time. The token is a JWS in
// the envelope's flattened JSON form, signed by the issuer, with its own
// typ, so a token is never taken for an envelope or the reverse:
//
//	{"protected": base64url({"alg": …, "kid": …, "typ": "ulp-delegation+jws"}),
//	 "payload":   base64url({"tokenId": …, "issuer": …, "audience": …,
//	                         "grantee": …, "scope": [...], "bookingRef": …,
//	                         "issuedAt": …, "expiresAt": …}),
//	 "signature": …}
//
// It travels as an envelope's attachments.delegation, signed again by the
// sender with the envelope. Parse checks the form only. Verify looks up the
// key the claimed issuer publishes under the token's kid itself (so no
// other key can be passed in by mistake), checks that its algorithm is the
// token's, and checks the signature; only the Verified it returns can be
// Checked against a request, and Check names the issuer the caller expects
// (the booking's shipper): a grant from anyone else grants nothing.
package delegation

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/internal/logsafe"
	"github.com/go-steer/supercargo/pkg/internal/strictjson"
	"github.com/go-steer/supercargo/pkg/sig"
)

const (
	// Type is a delegation token's JWS typ.
	Type = "ulp-delegation+jws"

	// MaxSize is the largest serialized token Parse accepts, in bytes.
	MaxSize = 4 << 10

	// MaxTTL bounds a grant's life: a token handed over can't be taken
	// back (short of rotating the issuer's key), so access always ends,
	// and soon.
	MaxTTL = 31 * 24 * time.Hour
)

// Scopes a grant may carry.
const (
	// ScopeRead lets the grantee ask about the booking: an inquiry, a
	// status request.
	ScopeRead = "tnt:read"
	// ScopeSubscribe lets the grantee subscribe to the booking's events
	// and alerts, pushed to its own endpoint.
	ScopeSubscribe = "tnt:subscribe"
)

var scopes = []string{ScopeRead, ScopeSubscribe}

// Scopes are every scope, in the order a grant lists them (a copy).
func Scopes() []string { return slices.Clone(scopes) }

var (
	// ErrMalformed reports a token that isn't well formed.
	ErrMalformed = errors.New("delegation: malformed")
	// ErrBadSignature reports a key that can't be found, a signature that
	// doesn't verify, or a key whose algorithm isn't the token's.
	ErrBadSignature = errors.New("delegation: bad signature")
	// ErrDenied reports a verified grant that doesn't cover the request:
	// another issuer, audience, grantee or booking, a scope it lacks, not
	// yet valid or expired.
	ErrDenied = errors.New("delegation: not granted")
)

// Grant is what a token says: Issuer (the booking's shipper) lets Grantee
// use Scope on booking BookingRef with Audience (its carrier), from
// IssuedAt until ExpiresAt (UTC, whole seconds). TokenID (a random UUID)
// names the grant in logs, and a later revocation. A Grant is what an
// issuer seals; one read from a token is only ever handed out Verified.
type Grant struct {
	TokenID    string   `json:"tokenId"`
	Issuer     string   `json:"issuer"`
	Audience   string   `json:"audience"`
	Grantee    string   `json:"grantee"`
	Scope      []string `json:"scope"`
	BookingRef string   `json:"bookingRef"`
	IssuedAt   string   `json:"issuedAt"`
	ExpiresAt  string   `json:"expiresAt"`
}

var (
	// refPattern is a booking reference as envelopes carry it (intents' rule).
	refPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
)

// Validate checks the grant's fields: a v4 UUID, three different parties'
// domains, one or both known scopes, each once, in order, a booking
// reference, and a life of at most MaxTTL.
func (g *Grant) Validate() error {
	if !uuidPattern.MatchString(g.TokenID) {
		return fmt.Errorf("tokenId %s is not a lowercase v4 UUID", logsafe.Quote(g.TokenID))
	}
	for _, f := range []struct{ name, v string }{{"issuer", g.Issuer}, {"audience", g.Audience}, {"grantee", g.Grantee}} {
		if !envelope.ValidDomain(f.v) {
			return fmt.Errorf("%s %s is not a lowercase domain", f.name, logsafe.Quote(f.v))
		}
	}
	if g.Issuer == g.Audience || g.Issuer == g.Grantee || g.Audience == g.Grantee {
		return errors.New("issuer, audience and grantee must be three different parties")
	}
	if len(g.Scope) == 0 || len(g.Scope) > len(scopes) {
		return fmt.Errorf("scope must list 1 to %d scopes", len(scopes))
	}
	last := -1
	for _, s := range g.Scope {
		i := slices.Index(scopes, s)
		if i < 0 {
			return fmt.Errorf("scope %s is not one of %v", logsafe.Quote(s), scopes)
		}
		if i <= last {
			return errors.New("scope lists each scope once, in order")
		}
		last = i
	}
	if !refPattern.MatchString(g.BookingRef) {
		return fmt.Errorf("bookingRef %s is not 1-64 of [A-Za-z0-9_-]", logsafe.Quote(g.BookingRef))
	}
	iat, ok := utcSecond(g.IssuedAt)
	if !ok {
		return fmt.Errorf("issuedAt %s is not a UTC time to the second", logsafe.Quote(g.IssuedAt))
	}
	exp, ok := utcSecond(g.ExpiresAt)
	if !ok {
		return fmt.Errorf("expiresAt %s is not a UTC time to the second", logsafe.Quote(g.ExpiresAt))
	}
	if !exp.After(iat) || exp.Sub(iat) > MaxTTL {
		return fmt.Errorf("expiresAt must be after issuedAt, by at most %s", MaxTTL)
	}
	return nil
}

// utcSecond parses s as a UTC time to the second, exactly: time.Parse
// would also take fractional seconds the layout doesn't have, so a time
// is one only if it formats back to s.
func utcSecond(s string) (time.Time, bool) {
	t, err := time.Parse(envelope.TimeLayout, s)
	return t, err == nil && t.Format(envelope.TimeLayout) == s
}

// New is a grant from issuer to grantee for bookingRef with audience,
// issued at now and lasting ttl (whole seconds, UTC), with a fresh token
// ID; scope must be known scopes (put in order), and the grant valid.
func New(issuer, audience, grantee, bookingRef string, scope []string, now time.Time, ttl time.Duration) (*Grant, error) {
	for _, s := range scope {
		if !slices.Contains(scopes, s) {
			return nil, fmt.Errorf("%w: scope %s is not one of %v", ErrMalformed, logsafe.Quote(s), scopes)
		}
	}
	var s []string
	for _, x := range scopes {
		if slices.Contains(scope, x) {
			s = append(s, x)
		}
	}
	now = now.UTC().Truncate(time.Second)
	g := &Grant{TokenID: newUUID(), Issuer: issuer, Audience: audience, Grantee: grantee, Scope: s, BookingRef: bookingRef,
		IssuedAt: now.Format(envelope.TimeLayout), ExpiresAt: now.Add(ttl.Truncate(time.Second)).Format(envelope.TimeLayout)}
	if err := g.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	return g, nil
}

// newUUID is a random (version 4) UUID in canonical lowercase form.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Unverified is a parsed, well-formed token whose signature has not been
// checked: only its claimed issuer and kid are offered, to find the key.
type Unverified struct {
	alg, kid     string
	grant        Grant
	signingInput []byte
	signature    []byte
}

// Issuer is the claimed issuer.
func (u *Unverified) Issuer() string { return u.grant.Issuer }

// KID is the protected header's kid.
func (u *Unverified) KID() string { return u.kid }

// KeyFunc finds the verifier a domain publishes under a kid (a node's key
// cache, *discovery.Cache).
type KeyFunc func(ctx context.Context, domain, kid string) (sig.Verifier, error)

// Verify checks the token's signature with the key its claimed issuer
// publishes under its kid, which keys finds; the key's algorithm must be
// the token's. Every failure wraps ErrBadSignature. A Verified grant is
// still to be Checked against a request.
func (u *Unverified) Verify(ctx context.Context, keys KeyFunc) (*Verified, error) {
	if u == nil || u.alg == "" || len(u.signature) == 0 {
		return nil, fmt.Errorf("%w: not a parsed token", ErrBadSignature)
	}
	if keys == nil {
		return nil, fmt.Errorf("%w: no key lookup", ErrBadSignature)
	}
	v, err := keys(ctx, u.grant.Issuer, u.kid)
	if err != nil {
		return nil, fmt.Errorf("%w: the issuer's key %s: %w", ErrBadSignature, logsafe.Quote(u.kid), err)
	}
	if v == nil {
		return nil, fmt.Errorf("%w: no key", ErrBadSignature)
	}
	if v.Alg() != u.alg {
		return nil, fmt.Errorf("%w: token names alg %s, key is %s", ErrBadSignature, logsafe.Quote(u.alg), logsafe.Quote(v.Alg()))
	}
	if err := v.Verify(u.signingInput, u.signature); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadSignature, err)
	}
	g := u.grant
	g.Scope = slices.Clone(g.Scope)
	return &Verified{g: g}, nil
}

// Verified is a grant whose issuer signed it. Only a Verified can be
// Checked.
type Verified struct{ g Grant }

// TokenID is the grant's token ID.
func (v *Verified) TokenID() string { return v.g.TokenID }

// Issuer is the grant's issuer.
func (v *Verified) Issuer() string { return v.g.Issuer }

// Audience is the grant's audience.
func (v *Verified) Audience() string { return v.g.Audience }

// Grantee is the grant's grantee.
func (v *Verified) Grantee() string { return v.g.Grantee }

// BookingRef is the grant's booking.
func (v *Verified) BookingRef() string { return v.g.BookingRef }

// Scope is the grant's scopes (a copy).
func (v *Verified) Scope() []string { return slices.Clone(v.g.Scope) }

// Allows reports whether the grant carries scope.
func (v *Verified) Allows(scope string) bool { return slices.Contains(v.g.Scope, scope) }

// Expires is when the grant ends.
func (v *Verified) Expires() time.Time {
	t, _ := utcSecond(v.g.ExpiresAt) // validated when parsed
	return t
}

// Check holds the grant to one request: issued by issuer (the booking's
// shipper, from the caller's own record), addressed to audience, used by
// grantee, for booking bookingRef, carrying scope, and valid at now
// (allowing skew either way, as for an envelope's sentAt). Every failure
// wraps ErrDenied; the reason is for the log, not the caller.
func (v *Verified) Check(issuer, audience, grantee, bookingRef, scope string, now time.Time, skew time.Duration) error {
	g := v.g
	iat, _ := utcSecond(g.IssuedAt) // validated when parsed
	exp, _ := utcSecond(g.ExpiresAt)
	q := logsafe.Quote
	switch {
	case g.Issuer != issuer:
		return fmt.Errorf("%w: issued by %s, not %s", ErrDenied, q(g.Issuer), q(issuer))
	case g.Audience != audience:
		return fmt.Errorf("%w: audience %s, not %s", ErrDenied, q(g.Audience), q(audience))
	case g.Grantee != grantee:
		return fmt.Errorf("%w: granted to %s, used by %s", ErrDenied, q(g.Grantee), q(grantee))
	case g.BookingRef != bookingRef:
		return fmt.Errorf("%w: for booking %s, not %s", ErrDenied, q(g.BookingRef), q(bookingRef))
	case !v.Allows(scope):
		return fmt.Errorf("%w: scope lacks %s", ErrDenied, q(scope))
	case now.Add(skew).Before(iat):
		return fmt.Errorf("%w: not valid until %s", ErrDenied, g.IssuedAt)
	case !now.Add(-skew).Before(exp):
		return fmt.Errorf("%w: expired %s", ErrDenied, g.ExpiresAt)
	}
	return nil
}

var (
	jwsMembers       = []string{"protected", "payload", "signature"}
	protectedMembers = []string{"alg", "kid", "typ"}
	grantMembers     = []string{"tokenId", "issuer", "audience", "grantee", "scope", "bookingRef", "issuedAt", "expiresAt"}
	algPattern       = regexp.MustCompile(`^[A-Za-z0-9._-]{1,32}$`)
)

// Parse decodes a token (an envelope's attachments.delegation) and checks
// that it is well formed: strict JSON, canonical unpadded base64url,
// exactly the defined members with the right types, a protected header
// naming Type, a well-formed kid and a signature alg (not "none"), and a
// payload whose grant passes Validate. Every error wraps ErrMalformed.
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
	enc, err := stringMembers(jws, "JWS", jwsMembers)
	if err != nil {
		return nil, err
	}
	rawProtected, err := sig.DecodeBase64URL(enc["protected"])
	if err != nil {
		return nil, fmt.Errorf("JWS protected header: %w", err)
	}
	ph, err := strictjson.DecodeObject(rawProtected)
	if err != nil {
		return nil, fmt.Errorf("JWS protected header: %w", err)
	}
	h, err := stringMembers(ph, "JWS protected header", protectedMembers)
	if err != nil {
		return nil, err
	}
	switch {
	case h["typ"] != Type:
		return nil, fmt.Errorf("JWS typ %s is not %q", logsafe.Quote(h["typ"]), Type)
	case !sig.ValidKID(h["kid"]):
		return nil, fmt.Errorf("JWS kid %s is not 1-64 characters of [A-Za-z0-9._-]", logsafe.Quote(h["kid"]))
	case !algPattern.MatchString(h["alg"]) || h["alg"] == "none":
		return nil, fmt.Errorf("JWS alg %s is not a signature algorithm identifier", logsafe.Quote(h["alg"]))
	}
	payload, err := sig.DecodeBase64URL(enc["payload"])
	if err != nil {
		return nil, fmt.Errorf("JWS payload: %w", err)
	}
	members, err := strictjson.DecodeObject(payload)
	if err != nil {
		return nil, fmt.Errorf("grant: %w", err)
	}
	if err := exact(members, "grant", grantMembers); err != nil {
		return nil, err
	}
	var g Grant
	// The member names are known to be exactly these by now, so
	// encoding/json's case-insensitive matching can't pick another.
	if err := json.Unmarshal(payload, &g); err != nil {
		return nil, fmt.Errorf("grant: %w", err)
	}
	if err := g.Validate(); err != nil {
		return nil, fmt.Errorf("grant: %w", err)
	}
	signature, err := sig.DecodeBase64URL(enc["signature"])
	if err != nil {
		return nil, fmt.Errorf("JWS signature: %w", err)
	}
	if len(signature) == 0 {
		return nil, errors.New("JWS signature is empty")
	}
	return &Unverified{alg: h["alg"], kid: h["kid"], grant: g,
		signingInput: []byte(enc["protected"] + "." + enc["payload"]), signature: signature}, nil
}

// stringMembers is m's members, exactly members, each a string.
func stringMembers(m map[string]any, name string, members []string) (map[string]string, error) {
	if err := exact(m, name, members); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(members))
	for _, k := range members {
		s, ok := m[k].(string)
		if !ok {
			return nil, fmt.Errorf("%s %s is not a string", name, k)
		}
		out[k] = s
	}
	return out, nil
}

// exact checks that m has exactly members.
func exact(m map[string]any, name string, members []string) error {
	for _, k := range members {
		if _, ok := m[k]; !ok {
			return fmt.Errorf("%s lacks %s", name, k)
		}
	}
	if len(m) != len(members) {
		for k := range m {
			if !slices.Contains(members, k) {
				return fmt.Errorf("%s has an unknown member %s", name, logsafe.Quote(k))
			}
		}
	}
	return nil
}

// Seal validates g and returns it as a token signed by s, which must be the
// key the issuer publishes under kid.
func Seal(g *Grant, kid string, s sig.Signer) ([]byte, error) {
	if s == nil {
		return nil, errors.New("delegation: no signer")
	}
	if err := g.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	if !sig.ValidKID(kid) {
		return nil, fmt.Errorf("%w: kid %s is not 1-64 characters of [A-Za-z0-9._-]", ErrMalformed, logsafe.Quote(kid))
	}
	if !algPattern.MatchString(s.Alg()) || s.Alg() == "none" {
		return nil, fmt.Errorf("delegation: signer alg %s is not a signature algorithm identifier", logsafe.Quote(s.Alg()))
	}
	ph, err := json.Marshal(struct {
		Alg string `json:"alg"`
		KID string `json:"kid"`
		Typ string `json:"typ"`
	}{s.Alg(), kid, Type})
	if err != nil {
		return nil, fmt.Errorf("delegation: %w", err)
	}
	payload, err := json.Marshal(g)
	if err != nil {
		return nil, fmt.Errorf("delegation: %w", err)
	}
	p, pl := base64.RawURLEncoding.EncodeToString(ph), base64.RawURLEncoding.EncodeToString(payload)
	signature, err := s.Sign([]byte(p + "." + pl))
	if err != nil {
		return nil, fmt.Errorf("delegation: signing: %w", err)
	}
	return json.Marshal(struct {
		Protected string `json:"protected"`
		Payload   string `json:"payload"`
		Signature string `json:"signature"`
	}{p, pl, base64.RawURLEncoding.EncodeToString(signature)})
}
