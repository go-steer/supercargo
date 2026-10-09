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

package envelope

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/supercargo/pkg/sig"
)

const kid = "cy-2026-1"

var now = time.Date(2026, 10, 9, 14, 3, 11, 0, time.UTC)

func ptr(s string) *string { return &s }

// keys mints a key pair in-process: no key material is checked in.
func keys(t *testing.T) (sig.Signer, sig.Verifier) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := sig.NewEd25519Signer(priv)
	if err != nil {
		t.Fatal(err)
	}
	v, err := sig.NewEd25519Verifier(pub)
	if err != nil {
		t.Fatal(err)
	}
	return s, v
}

// newInquiry is a valid envelope, with a compact body so that it survives
// Seal's re-encoding byte for byte.
func newInquiry() *Envelope {
	h := NewHeader("ulp.tnt.inquiry", "cymbal-shoes.example", "ocean-blue.example", now)
	h.BookingRef = ptr("OB-BKG-1042")
	return &Envelope{
		Header: h,
		Body:   json.RawMessage(`{"question":"What caused the delay?","n":1,"f":17.0}`),
	}
}

func seal(t *testing.T, e *Envelope, s sig.Signer) []byte {
	t.Helper()
	b, err := Seal(e, kid, s)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	return b
}

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

// rawJWS signs arbitrary protected-header and payload text, so tests can
// hand Parse correctly signed input that is wrong in some other way.
func rawJWS(t *testing.T, s sig.Signer, protectedJSON, payload string) string {
	t.Helper()
	input := b64(protectedJSON) + "." + b64(payload)
	signature, err := s.Sign([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	w, _ := json.Marshal(wire{Protected: b64(protectedJSON), Payload: b64(payload), Signature: base64.RawURLEncoding.EncodeToString(signature)})
	return string(w)
}

const goodProtected = `{"alg":"Ed25519","kid":"cy-2026-1","typ":"ulp-envelope+jws"}`

func payloadOf(t *testing.T, e *Envelope) string {
	t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

type golden struct {
	Keys    []json.RawMessage `json:"keys"`
	Vectors []struct {
		Name    string  `json:"name"`
		Wire    string  `json:"wire"`
		Payload *string `json:"payload"`
		Error   *string `json:"error"`
	} `json:"vectors"`
}

// TestGolden checks envelopes made by joserfc, an independent Python JOSE
// implementation: valid ones verify and yield exactly the payload bytes
// that were signed; the rest fail at the expected step.
func TestGolden(t *testing.T) {
	data, err := os.ReadFile("../../testdata/golden/envelope.json")
	if err != nil {
		t.Fatal(err)
	}
	var g golden
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatal(err)
	}
	verifiers := map[string]sig.Verifier{}
	for _, k := range g.Keys {
		id, v, err := sig.ParseJWK(k)
		if err != nil {
			t.Fatalf("ParseJWK(%s): %v", k, err)
		}
		verifiers[id] = v
	}
	seen := map[string]int{}
	for _, vec := range g.Vectors {
		t.Run(vec.Name, func(t *testing.T) {
			want := "ok"
			if vec.Error != nil {
				want = *vec.Error
			}
			seen[want]++
			u, err := Parse([]byte(vec.Wire))
			if want == "malformed" {
				if !errors.Is(err, ErrMalformed) || u != nil {
					t.Fatalf("Parse = %v, %v; want ErrMalformed", u, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			v, ok := verifiers[u.KID()]
			if !ok {
				t.Fatalf("no key %q in the golden file", u.KID())
			}
			env, err := u.Verify(v)
			if want == "signature" {
				if !errors.Is(err, ErrBadSignature) || env != nil {
					t.Fatalf("Verify = %v, %v; want ErrBadSignature", env, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			var exp Envelope
			if err := json.Unmarshal([]byte(*vec.Payload), &exp); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(env.Header, exp.Header) {
				t.Errorf("header = %+v, want %+v", env.Header, exp.Header)
			}
			// The body is the signed bytes as sent, not a re-encoding.
			if !bytes.Equal(env.Body, exp.Body) || !bytes.Contains([]byte(*vec.Payload), env.Body) {
				t.Errorf("body = %s, want the signed bytes %s", env.Body, exp.Body)
			}
		})
	}
	if seen["ok"] == 0 || seen["signature"] == 0 || seen["malformed"] == 0 {
		t.Fatalf("golden file outcomes %v; want valid, bad-signature and malformed vectors", seen)
	}
}

func TestRoundTrip(t *testing.T) {
	s, v := keys(t)
	e := newInquiry()
	e.Attachments.Delegation = json.RawMessage(`{"protected":"e30","payload":"e30","signature":"AA"}`)
	u, err := Parse(seal(t, e, s))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if u.Sender() != "cymbal-shoes.example" || u.KID() != kid || u.Alg() != sig.Ed25519 {
		t.Errorf("Sender/KID/Alg = %q/%q/%q", u.Sender(), u.KID(), u.Alg())
	}
	got, err := u.Verify(v)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !reflect.DeepEqual(got, e) {
		t.Errorf("round trip changed the envelope:\n got %+v\nwant %+v", got, e)
	}
}

func TestWireShape(t *testing.T) {
	s, _ := keys(t)
	var w map[string]string
	if err := json.Unmarshal(seal(t, newInquiry(), s), &w); err != nil {
		t.Fatal(err)
	}
	if len(w) != 3 || w["protected"] == "" || w["payload"] == "" || w["signature"] == "" {
		t.Fatalf("wire = %v, want exactly protected, payload and signature", w)
	}
	ph, err := sig.DecodeBase64URL(w["protected"])
	if err != nil || string(ph) != goodProtected {
		t.Errorf("protected header = %s, %v; want %s", ph, err, goodProtected)
	}
}

func TestVerifyReturnsACopy(t *testing.T) {
	s, v := keys(t)
	u, err := Parse(seal(t, newInquiry(), s))
	if err != nil {
		t.Fatal(err)
	}
	first, _ := u.Verify(v)
	first.Body[2] = 'X'
	*first.Header.BookingRef = "changed"
	second, err := u.Verify(v)
	if err != nil {
		t.Fatal(err)
	}
	want := newInquiryWithIDs(first.Header)
	if !reflect.DeepEqual(second.Header, want.Header) || !bytes.Equal(second.Body, want.Body) {
		t.Errorf("modifying one verified envelope changed the next: %+v", second)
	}
	claimed := u.ClaimedHeader()
	*claimed.BookingRef = "changed"
	if *u.ClaimedHeader().BookingRef != "OB-BKG-1042" {
		t.Error("ClaimedHeader shares the bookingRef")
	}
}

// newInquiryWithIDs is newInquiry with the message IDs of h, for comparing
// against an envelope that went through Seal.
func newInquiryWithIDs(h Header) *Envelope {
	e := newInquiry()
	e.Header.MessageID, e.Header.CorrelationID = h.MessageID, h.CorrelationID
	return e
}

// acceptAll verifies anything: a stand-in for a weak or wrong key.
type acceptAll struct{ alg string }

func (a acceptAll) Alg() string { return a.alg }

func (acceptAll) Verify(_, _ []byte) error { return nil }

func TestVerifyRejects(t *testing.T) {
	s, _ := keys(t)
	_, other := keys(t)
	u, err := Parse(seal(t, newInquiry(), s))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		v    sig.Verifier
	}{
		{"nil key", nil},
		{"other key", other},
		// The key decides the algorithm: even a verifier that accepts
		// everything is refused when its alg is not the message's.
		{"key for another alg", acceptAll{"EdDSA"}},
		{"key for alg none", acceptAll{"none"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, err := u.Verify(tc.v)
			if !errors.Is(err, ErrBadSignature) || env != nil {
				t.Errorf("Verify = %v, %v; want ErrBadSignature", env, err)
			}
		})
	}
}

// TestVerifyRejectsTampering changes the payload or protected header after
// signing, to something still well formed, and keeps the signature.
func TestVerifyRejectsTampering(t *testing.T) {
	s, v := keys(t)
	e := newInquiry()
	var w wire
	_ = json.Unmarshal(seal(t, e, s), &w)
	payload := payloadOf(t, e)

	swap := func(old, repl string) func(*wire) {
		return func(w *wire) {
			if !strings.Contains(payload, old) {
				t.Fatalf("payload has no %q", old)
			}
			w.Payload = b64(strings.Replace(payload, old, repl, 1))
		}
	}
	for _, tc := range []struct {
		name   string
		mutate func(*wire)
	}{
		{"intent", swap(`"ulp.tnt.inquiry"`, `"ulp.booking.request"`)},
		{"messageId", swap(e.Header.MessageID+`","correlationId"`, newUUID()+`","correlationId"`)},
		{"sentAt", swap(`14:03:11Z`, `14:03:12Z`)},
		{"sender", swap(`"cymbal-shoes.example"`, `"mallory.example"`)},
		{"receiver", swap(`"ocean-blue.example"`, `"mallory.example"`)},
		{"bookingRef", swap(`"OB-BKG-1042"`, `"OB-BKG-9999"`)},
		{"bookingRef removed", swap(`"OB-BKG-1042"`, `null`)},
		{"body value", swap(`delay?`, `delay!`)},
		{"body float to int", swap(`17.0`, `17`)},
		{"body reformatted", swap(`"n":1`, `"n": 1`)},
		{"delegation added", swap(`"delegation":null`, `"delegation":{}`)},
		{"kid", func(w *wire) { w.Protected = b64(strings.Replace(goodProtected, kid, "cy-2026-2", 1)) }},
		{"protected reformatted", func(w *wire) { w.Protected = b64(strings.Replace(goodProtected, `,`, `, `, 1)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tw := w
			tc.mutate(&tw)
			b, _ := json.Marshal(tw)
			u, err := Parse(b)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if env, err := u.Verify(v); !errors.Is(err, ErrBadSignature) || env != nil {
				t.Errorf("Verify = %v, %v; want ErrBadSignature", env, err)
			}
		})
	}
}

// TestParseRejects feeds Parse input that is wrong in exactly one way. The
// payload and protected-header cases are correctly signed, so Parse must
// reject them on form, before any key is involved.
func TestParseRejects(t *testing.T) {
	s, _ := keys(t)
	e := newInquiry()
	good := string(seal(t, e, s))
	payload := payloadOf(t, e)
	var w map[string]string
	_ = json.Unmarshal([]byte(good), &w)

	jwsEdit := func(f func(m map[string]any)) string {
		m := map[string]any{}
		for k, v := range w {
			m[k] = v
		}
		f(m)
		b, _ := json.Marshal(m)
		return string(b)
	}
	signedPayload := func(old, repl string) string {
		if !strings.Contains(payload, old) {
			t.Fatalf("payload has no %q", old)
		}
		return rawJWS(t, s, goodProtected, strings.Replace(payload, old, repl, 1))
	}
	signedProtected := func(p string) string { return rawJWS(t, s, p, payload) }

	for _, tc := range []struct {
		name string
		in   string
	}{
		// The JWS object.
		{"empty", ""},
		{"not JSON", "envelope"},
		{"array", "[]"},
		{"compact serialization", `"` + w["protected"] + "." + w["payload"] + "." + w["signature"] + `"`},
		{"invalid UTF-8", good[:len(good)-1] + "\xff}"},
		{"trailing data", good + "{}"},
		{"duplicate member", strings.Replace(good, `"signature":`, `"payload":"e30","signature":`, 1)},
		{"unprotected header", jwsEdit(func(m map[string]any) { m["header"] = map[string]any{"kid": "x"} })},
		{"general serialization", jwsEdit(func(m map[string]any) { m["signatures"] = []any{}; delete(m, "signature") })},
		{"missing protected", jwsEdit(func(m map[string]any) { delete(m, "protected") })},
		{"missing payload", jwsEdit(func(m map[string]any) { delete(m, "payload") })},
		{"missing signature", jwsEdit(func(m map[string]any) { delete(m, "signature") })},
		{"payload not a string", jwsEdit(func(m map[string]any) { m["payload"] = map[string]any{} })},
		{"payload padded", jwsEdit(func(m map[string]any) { m["payload"] = w["payload"] + "=" })},
		{"payload with newline", jwsEdit(func(m map[string]any) { m["payload"] = w["payload"][:8] + "\n" + w["payload"][8:] })},
		{"payload standard base64", jwsEdit(func(m map[string]any) { m["payload"] = w["payload"] + "+" })},
		{"signature empty", jwsEdit(func(m map[string]any) { m["signature"] = "" })},
		{"signature padded", jwsEdit(func(m map[string]any) { m["signature"] = w["signature"] + "==" })},
		{"protected not base64", jwsEdit(func(m map[string]any) { m["protected"] = "!!" })},

		// The protected header, correctly signed.
		{"protected not JSON", signedProtected(`alg`)},
		{"protected array", signedProtected(`[]`)},
		{"protected duplicate alg", signedProtected(`{"alg":"none","alg":"Ed25519","kid":"cy-2026-1","typ":"ulp-envelope+jws"}`)},
		{"protected no kid", signedProtected(`{"alg":"Ed25519","typ":"ulp-envelope+jws"}`)},
		{"protected no typ", signedProtected(`{"alg":"Ed25519","kid":"cy-2026-1"}`)},
		{"protected no alg", signedProtected(`{"kid":"cy-2026-1","typ":"ulp-envelope+jws"}`)},
		{"protected crit", signedProtected(`{"alg":"Ed25519","kid":"cy-2026-1","typ":"ulp-envelope+jws","crit":["exp"],"exp":1}`)},
		{"protected jku", signedProtected(`{"alg":"Ed25519","kid":"cy-2026-1","typ":"ulp-envelope+jws","jku":"https://evil.example/keys"}`)},
		{"typ JWT", signedProtected(`{"alg":"Ed25519","kid":"cy-2026-1","typ":"JWT"}`)},
		{"typ delegation", signedProtected(`{"alg":"Ed25519","kid":"cy-2026-1","typ":"ulp-delegation+jws"}`)},
		{"typ uppercase", signedProtected(`{"alg":"Ed25519","kid":"cy-2026-1","typ":"ULP-ENVELOPE+JWS"}`)},
		{"kid empty", signedProtected(`{"alg":"Ed25519","kid":"","typ":"ulp-envelope+jws"}`)},
		{"kid path", signedProtected(`{"alg":"Ed25519","kid":"../../keys","typ":"ulp-envelope+jws"}`)},
		{"kid not string", signedProtected(`{"alg":"Ed25519","kid":1,"typ":"ulp-envelope+jws"}`)},
		{"alg empty", signedProtected(`{"alg":"","kid":"cy-2026-1","typ":"ulp-envelope+jws"}`)},
		{"alg with space", signedProtected(`{"alg":"Ed 25519","kid":"cy-2026-1","typ":"ulp-envelope+jws"}`)},
		{"alg too long", signedProtected(`{"alg":"` + strings.Repeat("A", 33) + `","kid":"cy-2026-1","typ":"ulp-envelope+jws"}`)},
		{"alg not string", signedProtected(`{"alg":null,"kid":"cy-2026-1","typ":"ulp-envelope+jws"}`)},

		// The payload, correctly signed.
		{"payload not JSON", rawJWS(t, s, goodProtected, "payload")},
		{"payload array", rawJWS(t, s, goodProtected, "[]")},
		{"payload invalid UTF-8", rawJWS(t, s, goodProtected, strings.Replace(payload, "What", "Wh\xffat", 1))},
		{"payload trailing data", rawJWS(t, s, goodProtected, payload+"{}")},
		{"duplicate body", signedPayload(`"attachments":`, `"body":{},"attachments":`)},
		{"case-variant member", signedPayload(`"body":`, `"Body":`)},
		{"case-variant duplicate", signedPayload(`"attachments":`, `"Body":{},"attachments":`)},
		{"unknown payload member", signedPayload(`"attachments":`, `"extra":1,"attachments":`)},
		{"missing header", rawJWS(t, s, goodProtected, `{"body":{},"attachments":{"delegation":null}}`)},
		{"header not an object", rawJWS(t, s, goodProtected, `{"header":"x","body":{},"attachments":{"delegation":null}}`)},
		{"missing header member", signedPayload(`"inReplyTo":null,`, ``)},
		{"kid in payload header", signedPayload(`"sender":`, `"kid":"cy-2026-1","sender":`)},
		{"alg in payload header", signedPayload(`"sender":`, `"alg":"Ed25519","sender":`)},
		{"duplicate header member", signedPayload(`"receiver":`, `"sender":"mallory.example","receiver":`)},
		{"null non-nullable header member", signedPayload(`"sentAt":"2026-10-09T14:03:11Z"`, `"sentAt":null`)},
		{"number header member", signedPayload(`"sentAt":"2026-10-09T14:03:11Z"`, `"sentAt":1760018591`)},
		{"array nullable header member", signedPayload(`"inReplyTo":null`, `"inReplyTo":["a"]`)},
		{"bad header value", signedPayload(`"sender":"cymbal-shoes.example"`, `"sender":"evil.example:8443"`)},
		{"wrong version", signedPayload(`"0.4-demo"`, `"0.1"`)},
		{"body array", signedPayload(`"body":{"question":"What caused the delay?","n":1,"f":17.0}`, `"body":[]`)},
		{"body null", signedPayload(`"body":{"question":"What caused the delay?","n":1,"f":17.0}`, `"body":null`)},
		{"attachments null", signedPayload(`"attachments":{"delegation":null}`, `"attachments":null`)},
		{"attachments unknown member", signedPayload(`{"delegation":null}`, `{"delegation":null,"file":null}`)},
		{"attachments missing delegation", signedPayload(`{"delegation":null}`, `{}`)},
		{"delegation string", signedPayload(`"delegation":null`, `"delegation":"token"`)},
		{"lone surrogate in body", signedPayload(`"What caused`, `"\ud800What caused`)},
		{"deep nesting in body", signedPayload(`"n":1`, `"n":`+strings.Repeat("[", 65)+strings.Repeat("]", 65))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.in == good {
				t.Fatal("case did not change the envelope")
			}
			u, err := Parse([]byte(tc.in))
			if !errors.Is(err, ErrMalformed) || u != nil {
				t.Errorf("Parse = %+v, %v; want ErrMalformed", u, err)
			}
		})
	}
}

// TestParseSizeLimit uses a valid signed envelope padded to exactly
// MaxSize, so the only thing wrong one byte later is the size.
func TestParseSizeLimit(t *testing.T) {
	s, _ := keys(t)
	padded := func(size int) []byte {
		e := newInquiry()
		e.Body = json.RawMessage(`{"pad":""}`)
		short := len(seal(t, e, s))
		// base64 makes 4 characters of every 3 payload bytes; pad in steps
		// of 3 and then trim the remainder from the end of the padding.
		n := (size - short) / 4 * 3
		for {
			e.Body = json.RawMessage(`{"pad":"` + strings.Repeat("x", n) + `"}`)
			if b := seal(t, e, s); len(b) >= size {
				return b
			}
			n++
		}
	}
	at := padded(MaxSize - 2)
	if len(at) > MaxSize {
		t.Fatalf("could not build an envelope within MaxSize (%d)", len(at))
	}
	if _, err := Parse(at); err != nil {
		t.Errorf("Parse at %d bytes: %v", len(at), err)
	}
	over := padded(MaxSize + 1)
	_, err := Parse(over)
	if !errors.Is(err, ErrMalformed) || !strings.Contains(err.Error(), "byte limit") {
		t.Errorf("Parse at %d bytes = %v, want the size error", len(over), err)
	}
}

type badSigner struct {
	alg string
	err error
}

func (b badSigner) Alg() string { return b.alg }

func (b badSigner) Sign([]byte) ([]byte, error) { return []byte{1}, b.err }

func TestSealRejects(t *testing.T) {
	s, _ := keys(t)
	if _, err := Seal(newInquiry(), kid, nil); err == nil {
		t.Error("Seal with no signer succeeded")
	}
	bad := newInquiry()
	bad.Header.Sender = "Mallory.example"
	if _, err := Seal(bad, kid, s); !errors.Is(err, ErrMalformed) {
		t.Errorf("Seal of a bad header = %v, want ErrMalformed", err)
	}
	bad = newInquiry()
	bad.Body = json.RawMessage(`[1]`)
	if _, err := Seal(bad, kid, s); !errors.Is(err, ErrMalformed) {
		t.Errorf("Seal of an array body = %v, want ErrMalformed", err)
	}
	if _, err := Seal(newInquiry(), "bad kid", s); !errors.Is(err, ErrMalformed) {
		t.Errorf("Seal with a bad kid = %v, want ErrMalformed", err)
	}
	if _, err := Seal(newInquiry(), kid, badSigner{alg: "bad alg"}); err == nil {
		t.Error("Seal with a malformed signer alg succeeded")
	}
	if _, err := Seal(newInquiry(), kid, badSigner{alg: "X", err: errors.New("hsm offline")}); err == nil {
		t.Error("Seal with a failing signer succeeded")
	}
}

func TestNewHeader(t *testing.T) {
	h := NewHeader("ulp.booking.request", "cymbal-shoes.example", "ocean-blue.example",
		time.Date(2026, 10, 9, 16, 3, 11, 999, time.FixedZone("CEST", 2*3600)))
	if err := h.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if h.SentAt != "2026-10-09T14:03:11Z" {
		t.Errorf("SentAt = %q, want UTC whole seconds", h.SentAt)
	}
	if h.CorrelationID != h.MessageID || h.InReplyTo != nil || h.BookingRef != nil || h.ULPVersion != Version {
		t.Errorf("new conversation header = %+v", h)
	}
	if h2 := NewHeader("ulp.booking.request", "a.example", "b.example", now); h2.MessageID == h.MessageID {
		t.Error("two headers share a messageId")
	}
}

func TestReply(t *testing.T) {
	req := newInquiry().Header
	r := req.Reply("ulp.tnt.inquiry.response", now)
	if err := r.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if r.Sender != req.Receiver || r.Receiver != req.Sender {
		t.Errorf("parties not swapped: %s -> %s", r.Sender, r.Receiver)
	}
	if r.CorrelationID != req.CorrelationID || r.InReplyTo == nil || *r.InReplyTo != req.MessageID {
		t.Errorf("reply not threaded: %+v", r)
	}
	if r.MessageID == req.MessageID || r.Intent != "ulp.tnt.inquiry.response" {
		t.Errorf("reply header = %+v", r)
	}
	if r.BookingRef == nil || *r.BookingRef != "OB-BKG-1042" || r.BookingRef == req.BookingRef {
		t.Errorf("bookingRef not copied by value: %v", r.BookingRef)
	}
	if noRef := NewHeader("ulp.booking.request", "a.example", "b.example", now).Reply("ulp.error", now); noRef.BookingRef != nil {
		t.Errorf("reply invented a bookingRef: %v", *noRef.BookingRef)
	}
}

func TestSentTime(t *testing.T) {
	h := newInquiry().Header
	if got, err := h.SentTime(); err != nil || !got.Equal(now) {
		t.Errorf("SentTime = %v, %v; want %v", got, err, now)
	}
	h.SentAt = "yesterday"
	if _, err := h.SentTime(); !errors.Is(err, ErrMalformed) {
		t.Errorf("SentTime(bad) = %v, want ErrMalformed", err)
	}
}

func TestHeaderValidate(t *testing.T) {
	long := strings.Repeat("a", 64)
	for _, tc := range []struct {
		name   string
		mutate func(h *Header)
		ok     bool
	}{
		{"valid", func(h *Header) {}, true},
		{"valid reply", func(h *Header) { h.InReplyTo = ptr(newUUID()) }, true},
		{"valid no bookingRef", func(h *Header) { h.BookingRef = nil }, true},
		{"valid 64-char bookingRef", func(h *Header) { h.BookingRef = ptr(long) }, true},
		{"valid long intent", func(h *Header) { h.Intent = "ulp.tnt.inquiry.response" }, true},
		{"valid dialect", func(h *Header) {
			h.Dialect = &Dialect{"DCSA_TNT", "2.2.0", "https://dcsa.org/standards/track-and-trace"}
		}, true},

		{"dialect lowercase standard", func(h *Header) { h.Dialect = &Dialect{"dcsa_tnt", "2.2.0", "https://dcsa.org/x"} }, false},
		{"dialect empty standard", func(h *Header) { h.Dialect = &Dialect{"", "2.2.0", "https://dcsa.org/x"} }, false},
		{"dialect long standard", func(h *Header) { h.Dialect = &Dialect{"D" + strings.Repeat("A", 32), "2.2.0", "https://dcsa.org/x"} }, false},
		{"dialect word version", func(h *Header) { h.Dialect = &Dialect{"DCSA_TNT", "latest", "https://dcsa.org/x"} }, false},
		{"dialect http schema", func(h *Header) { h.Dialect = &Dialect{"DCSA_TNT", "2.2.0", "http://dcsa.org/x"} }, false},
		{"dialect schema with query", func(h *Header) { h.Dialect = &Dialect{"DCSA_TNT", "2.2.0", "https://dcsa.org/x?a=1"} }, false},
		{"dialect schema with userinfo", func(h *Header) { h.Dialect = &Dialect{"DCSA_TNT", "2.2.0", "https://u@dcsa.org/x"} }, false},
		{"dialect schema with space", func(h *Header) { h.Dialect = &Dialect{"DCSA_TNT", "2.2.0", "https://dcsa.org/a b"} }, false},
		{"dialect long schema", func(h *Header) {
			h.Dialect = &Dialect{"DCSA_TNT", "2.2.0", "https://dcsa.org/" + strings.Repeat("x", 250)}
		}, false},
		{"dialect empty schema", func(h *Header) { h.Dialect = &Dialect{"DCSA_TNT", "2.2.0", ""} }, false},

		{"empty version", func(h *Header) { h.ULPVersion = "" }, false},
		{"other version", func(h *Header) { h.ULPVersion = "0.1" }, false},
		{"empty intent", func(h *Header) { h.Intent = "" }, false},
		{"intent without ulp prefix", func(h *Header) { h.Intent = "tnt.inquiry" }, false},
		{"intent bare ulp", func(h *Header) { h.Intent = "ulp" }, false},
		{"intent uppercase", func(h *Header) { h.Intent = "ulp.TNT.inquiry" }, false},
		{"intent trailing dot", func(h *Header) { h.Intent = "ulp.tnt." }, false},
		{"intent too long", func(h *Header) { h.Intent = "ulp." + strings.Repeat("a", 61) }, false},
		{"messageId empty", func(h *Header) { h.MessageID = "" }, false},
		{"messageId uppercase", func(h *Header) { h.MessageID = strings.ToUpper(h.MessageID) }, false},
		{"messageId version 1", func(h *Header) { h.MessageID = "8f1c2e9a-5b7d-1f0e-9c3a-2d6b8e1f4a77" }, false},
		{"messageId bad variant", func(h *Header) { h.MessageID = "8f1c2e9a-5b7d-4f0e-7c3a-2d6b8e1f4a77" }, false},
		{"messageId braces", func(h *Header) { h.MessageID = "{8f1c2e9a-5b7d-4f0e-9c3a-2d6b8e1f4a7}" }, false},
		{"messageId no dashes", func(h *Header) { h.MessageID = "8f1c2e9a5b7d4f0e9c3a2d6b8e1f4a77abcd" }, false},
		{"messageId non-hex", func(h *Header) { h.MessageID = "8f1c2e9a-5b7d-4f0e-9c3a-2d6b8e1f4a7g" }, false},
		{"correlationId empty", func(h *Header) { h.CorrelationID = "" }, false},
		{"inReplyTo empty", func(h *Header) { h.InReplyTo = ptr("") }, false},
		{"inReplyTo not a UUID", func(h *Header) { h.InReplyTo = ptr("msg-1") }, false},
		{"sentAt empty", func(h *Header) { h.SentAt = "" }, false},
		{"sentAt offset", func(h *Header) { h.SentAt = "2026-10-09T16:03:11+02:00" }, false},
		{"sentAt fractional", func(h *Header) { h.SentAt = "2026-10-09T14:03:11.5Z" }, false},
		{"sentAt lowercase z", func(h *Header) { h.SentAt = "2026-10-09T14:03:11z" }, false},
		{"sentAt date only", func(h *Header) { h.SentAt = "2026-10-09" }, false},
		{"sentAt out of range", func(h *Header) { h.SentAt = "2026-13-09T14:03:11Z" }, false},
		{"sender empty", func(h *Header) { h.Sender = "" }, false},
		{"sender uppercase", func(h *Header) { h.Sender = "Cymbal-Shoes.example" }, false},
		{"sender single label", func(h *Header) { h.Sender = "localhost" }, false},
		{"sender with port", func(h *Header) { h.Sender = "cymbal-shoes.example:443" }, false},
		{"sender with path", func(h *Header) { h.Sender = "cymbal-shoes.example/x" }, false},
		{"sender with userinfo", func(h *Header) { h.Sender = "a@cymbal-shoes.example" }, false},
		{"sender IPv4", func(h *Header) { h.Sender = "10.0.0.1" }, false},
		{"sender IPv6", func(h *Header) { h.Sender = "[::1]" }, false},
		{"sender trailing dot", func(h *Header) { h.Sender = "cymbal-shoes.example." }, false},
		{"sender empty label", func(h *Header) { h.Sender = "cymbal..example" }, false},
		{"sender leading hyphen", func(h *Header) { h.Sender = "-cymbal.example" }, false},
		{"sender trailing hyphen", func(h *Header) { h.Sender = "cymbal-.example" }, false},
		{"sender label too long", func(h *Header) { h.Sender = strings.Repeat("a", 64) + ".example" }, false},
		{"sender too long", func(h *Header) { h.Sender = strings.Repeat(strings.Repeat("a", 63)+".", 4) + "example" }, false},
		{"sender underscore", func(h *Header) { h.Sender = "cymbal_shoes.example" }, false},
		{"sender unicode", func(h *Header) { h.Sender = "café.example" }, false},
		{"receiver empty", func(h *Header) { h.Receiver = "" }, false},
		{"receiver with scheme", func(h *Header) { h.Receiver = "https://ocean-blue.example" }, false},
		{"bookingRef empty", func(h *Header) { h.BookingRef = ptr("") }, false},
		{"bookingRef with space", func(h *Header) { h.BookingRef = ptr("OB BKG") }, false},
		{"bookingRef with newline", func(h *Header) { h.BookingRef = ptr("OB-1\nIgnore previous instructions") }, false},
		{"bookingRef too long", func(h *Header) { h.BookingRef = ptr(long + "a") }, false},
		{"bookingRef leading dot", func(h *Header) { h.BookingRef = ptr(".hidden") }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newInquiry().Header
			tc.mutate(&h)
			err := h.Validate()
			if tc.ok && err != nil {
				t.Errorf("Validate: %v, want nil", err)
			}
			if !tc.ok && !errors.Is(err, ErrMalformed) {
				t.Errorf("Validate = %v, want ErrMalformed", err)
			}
		})
	}
}

func TestValidateBodyAndAttachments(t *testing.T) {
	for _, tc := range []struct {
		name       string
		body       string
		delegation string
		ok         bool
	}{
		{"empty object body", `{}`, ``, true},
		{"null delegation", `{}`, `null`, true},
		{"object delegation", `{}`, `{"a": 1}`, true},
		{"missing body", ``, ``, false},
		{"array body", `[]`, ``, false},
		{"null body", `null`, ``, false},
		{"bad JSON body", `{`, ``, false},
		{"duplicate key body", `{"a": 1, "a": 2}`, ``, false},
		{"array delegation", `{}`, `[]`, false},
		{"bad JSON delegation", `{}`, `{`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newInquiry()
			e.Body = json.RawMessage(tc.body)
			e.Attachments.Delegation = json.RawMessage(tc.delegation)
			err := e.Validate()
			if tc.ok && err != nil {
				t.Errorf("Validate: %v, want nil", err)
			}
			if !tc.ok && !errors.Is(err, ErrMalformed) {
				t.Errorf("Validate = %v, want ErrMalformed", err)
			}
		})
	}
}

func TestNewUUID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		u := newUUID()
		if !isUUIDv4(u) || seen[u] {
			t.Fatalf("newUUID() = %q: not a fresh canonical UUIDv4", u)
		}
		seen[u] = true
	}
}

// TestZeroUnverified: only Parse makes an Unverified that can verify. A
// zero or nil one must not yield an Envelope, even with a verifier that
// accepts everything.
func TestZeroUnverified(t *testing.T) {
	for name, u := range map[string]*Unverified{"zero": {}, "nil": nil} {
		if env, err := u.Verify(acceptAll{""}); !errors.Is(err, ErrBadSignature) || env != nil {
			t.Errorf("%s Unverified: Verify = %v, %v; want ErrBadSignature", name, env, err)
		}
	}
}

func TestDelegation(t *testing.T) {
	s, v := keys(t)
	// No delegation on the wire (null) comes back as nil, not "null".
	u, err := Parse(seal(t, newInquiry(), s))
	if err != nil {
		t.Fatal(err)
	}
	env, _ := u.Verify(v)
	if env.Attachments.Delegation != nil {
		t.Errorf("absent delegation = %q, want nil", env.Attachments.Delegation)
	}
	// A present delegation is copied out, like the body.
	e := newInquiry()
	e.Attachments.Delegation = json.RawMessage(`{"protected":"e30","payload":"e30","signature":"AA"}`)
	u, err = Parse(seal(t, e, s))
	if err != nil {
		t.Fatal(err)
	}
	first, _ := u.Verify(v)
	first.Attachments.Delegation[2] = 'X'
	second, _ := u.Verify(v)
	if !bytes.Equal(second.Attachments.Delegation, e.Attachments.Delegation) {
		t.Errorf("modifying one verified delegation changed the next: %s", second.Attachments.Delegation)
	}
}

// TestErrorsAreBounded: rejecting a message must not echo megabytes of it
// into the error, which ends up in logs.
func TestErrorsAreBounded(t *testing.T) {
	s, _ := keys(t)
	huge := strings.Repeat("A", 60<<10) // under MaxSize once base64-encoded
	for _, in := range []string{
		rawJWS(t, s, `{"alg":"Ed25519","kid":"cy-2026-1","typ":"`+huge+`"}`, payloadOf(t, newInquiry())),
		rawJWS(t, s, `{"alg":"Ed25519","kid":"`+huge+`","typ":"ulp-envelope+jws"}`, payloadOf(t, newInquiry())),
		rawJWS(t, s, goodProtected, strings.Replace(payloadOf(t, newInquiry()), `"ulp.tnt.inquiry"`, `"`+huge+`"`, 1)),
		rawJWS(t, s, goodProtected, strings.Replace(payloadOf(t, newInquiry()), `"attachments":`, `"`+huge+`":1,"attachments":`, 1)),
	} {
		_, err := Parse([]byte(in))
		if err == nil {
			t.Fatal("Parse succeeded")
		}
		if strings.Contains(err.Error(), "byte limit") {
			t.Fatal("hit the size limit, not the error path under test")
		}
		if len(err.Error()) > 300 {
			t.Errorf("error is %d bytes: %.120s...", len(err.Error()), err)
		}
	}
}

func TestIPLikeSendersRejected(t *testing.T) {
	for _, sender := range []string{"127.0x1", "0177.0x1", "0x7f.0.0.0x1", "10.0.0.1", "1.2.3.4", "example.0x1", "example.1a", "a.-b"} {
		h := newInquiry().Header
		h.Sender = sender
		if err := h.Validate(); !errors.Is(err, ErrMalformed) {
			t.Errorf("sender %q: Validate = %v, want ErrMalformed", sender, err)
		}
	}
	for _, sender := range []string{"a1.example", "0x7f.example", "127.0.0.1.example", "xn--caf-dma.example"} {
		h := newInquiry().Header
		h.Sender = sender
		if err := h.Validate(); err != nil {
			t.Errorf("sender %q: Validate = %v, want nil", sender, err)
		}
	}
}

// TestSignatureTrailingBits: the signature's last base64url character
// carries 4 unused bits; setting one decodes to the same bytes, so only the
// canonical-encoding check refuses it.
func TestSignatureTrailingBits(t *testing.T) {
	s, _ := keys(t)
	var w wire
	_ = json.Unmarshal(seal(t, newInquiry(), s), &w)
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	last := strings.IndexByte(alphabet, w.Signature[len(w.Signature)-1])
	w.Signature = w.Signature[:len(w.Signature)-1] + string(alphabet[last+1])
	b, _ := json.Marshal(w)
	if u, err := Parse(b); !errors.Is(err, ErrMalformed) {
		t.Errorf("Parse = %v, %v; want ErrMalformed", u, err)
	}
}

// BenchmarkParseHostile is the worst unauthenticated input: an envelope
// near MaxSize whose body is as many JSON tokens as fit. Parse runs on it
// before any key is resolved, so this is the per-message cost an anonymous
// sender can impose.
func BenchmarkParseHostile(b *testing.B) {
	payload := `{"header":{"ulpVersion":"0.4-demo","intent":"ulp.tnt.inquiry","messageId":"8f1c2e9a-5b7d-4f0e-9c3a-2d6b8e1f4a77","correlationId":"8f1c2e9a-5b7d-4f0e-9c3a-2d6b8e1f4a77","inReplyTo":null,"sentAt":"2026-10-09T14:03:11Z","sender":"mallory.example","receiver":"ocean-blue.example","bookingRef":null},"body":{"a":[` +
		strings.TrimSuffix(strings.Repeat("1,", (MaxSize-1000)*3/4/2), ",") + `]},"attachments":{"delegation":null}}`
	w, _ := json.Marshal(wire{Protected: b64(goodProtected), Payload: b64(payload), Signature: strings.Repeat("A", 86)})
	if len(w) > MaxSize {
		b.Fatalf("hostile envelope is %d bytes, over MaxSize", len(w))
	}
	if _, err := Parse(w); err != nil {
		b.Fatalf("Parse: %v", err)
	}
	b.SetBytes(int64(len(w)))
	for b.Loop() {
		_, _ = Parse(w)
	}
}

// TestGoSealedEnvelopesForPython writes Go-sealed envelopes for the joserfc
// cross-check (dev/tools/gen-golden-vectors verify-go FILE). It only runs
// when SUPERCARGO_GO_SIGNED_OUT names the file to write.
func TestGoSealedEnvelopesForPython(t *testing.T) {
	out := os.Getenv("SUPERCARGO_GO_SIGNED_OUT")
	if out == "" {
		t.Skip("set SUPERCARGO_GO_SIGNED_OUT=FILE to write Go-sealed envelopes for the Python check")
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	s, _ := sig.NewEd25519Signer(priv)
	jwk, err := sig.Ed25519JWK(pub, kid)
	if err != nil {
		t.Fatal(err)
	}
	bodies := map[string]string{
		"plain":   `{"question":"What caused the delay?"}`,
		"numbers": `{"n":1,"f":17.0,"big":1e16,"small":1e-05,"huge":123456789012345678901234567890,"neg":-0.0}`,
		"unicode": `{"q":"Café 東京 😀 <&> \"quoted\"\n\t\u007f\u0001 — ETA?","é":[],"z":{}}`,
		"reply":   `{"a":{"b":[null,true,false,{"c":[1.5,"s"]}]}}`,
		"dialect": `{"event_id":"EV-1"}`,
	}
	type item struct {
		Name    string `json:"name"`
		Wire    string `json:"wire"`
		Payload string `json:"payload"`
	}
	var items []item
	for name, body := range bodies {
		e := newInquiry()
		e.Body = json.RawMessage(body)
		if name == "dialect" {
			e.Header.Intent = "ulp.tnt.event"
			e.Header.Dialect = &Dialect{"DCSA_TNT", "2.2.0", "https://dcsa.org/standards/track-and-trace"}
		}
		if name == "reply" {
			e.Header = e.Header.Reply("ulp.tnt.inquiry.response", now)
			e.Attachments.Delegation = json.RawMessage(`{"protected":"e30","payload":"e30","signature":"AA"}`)
		}
		w := seal(t, e, s)
		var parsed wire
		_ = json.Unmarshal(w, &parsed)
		p, _ := sig.DecodeBase64URL(parsed.Payload)
		items = append(items, item{name, string(w), string(p)})
	}
	doc, _ := json.MarshalIndent(map[string]any{"jwk": json.RawMessage(jwk), "envelopes": items}, "", "  ")
	if err := os.WriteFile(out, doc, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %d envelopes to %s", len(items), out)
}

// TestDialectShape: the optional dialect member is an object of exactly
// three strings, once, named exactly; anything else is malformed before
// any key is consulted.
func TestDialectShape(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	s, _ := sig.NewEd25519Signer(priv)
	e := newInquiry()
	e.Header.Dialect = &Dialect{"DCSA_TNT", "2.2.0", "https://dcsa.org/x"}
	good := seal(t, e, s)
	if _, err := Parse(good); err != nil {
		t.Fatalf("a valid dialect: %v", err)
	}
	var w wire
	_ = json.Unmarshal(good, &w)
	payload, _ := sig.DecodeBase64URL(w.Payload)
	const d = `"dialect":{"standard":"DCSA_TNT","version":"2.2.0","schema_url":"https://dcsa.org/x"}`
	if !strings.Contains(string(payload), d) {
		t.Fatalf("payload %s", payload)
	}
	for name, repl := range map[string]string{
		"case variant":        strings.Replace(d, `"dialect"`, `"Dialect"`, 1),
		"member case variant": strings.Replace(d, `"standard"`, `"Standard"`, 1),
		"missing member":      `"dialect":{"standard":"DCSA_TNT","version":"2.2.0"}`,
		"extra member":        strings.Replace(d, `}`, `,"x":"y"}`, 1),
		"string":              `"dialect":"DCSA_TNT"`,
		"array":               `"dialect":["DCSA_TNT"]`,
		"null":                `"dialect":null`,
		"duplicate":           d + "," + d,
		"bad standard":        strings.Replace(d, `"DCSA_TNT"`, `"dcsa"`, 1),
	} {
		forged := strings.Replace(string(payload), d, repl, 1)
		msg := resign(t, w.Protected, []byte(forged), s)
		if _, err := Parse(msg); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: Parse = %v, want ErrMalformed", name, err)
		}
	}
}

// TestClaimedHeaderDialectCopy: changing the claimed header's dialect
// changes nothing Verify returns.
func TestClaimedHeaderDialectCopy(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	s, _ := sig.NewEd25519Signer(priv)
	v, _ := sig.NewEd25519Verifier(pub)
	e := newInquiry()
	e.Header.Dialect = &Dialect{"DCSA_TNT", "2.2.0", "https://dcsa.org/x"}
	u, err := Parse(seal(t, e, s))
	if err != nil {
		t.Fatal(err)
	}
	u.ClaimedHeader().Dialect.Version = "9.9"
	env, err := u.Verify(v)
	if err != nil || env.Header.Dialect.Version != "2.2.0" {
		t.Errorf("verified dialect %+v, %v", env.Header.Dialect, err)
	}
}

// resign signs payload under the protected header b64, as a forger with
// the key would: shape checks must hold even for signed input.
func resign(t *testing.T, protectedB64 string, payload []byte, s sig.Signer) []byte {
	t.Helper()
	p := base64.RawURLEncoding.EncodeToString(payload)
	signature, err := s.Sign([]byte(protectedB64 + "." + p))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(map[string]string{"protected": protectedB64, "payload": p, "signature": base64.RawURLEncoding.EncodeToString(signature)})
	return out
}
