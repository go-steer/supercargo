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
	"crypto/rand"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/go-steer/supercargo/pkg/internal/logsafe"
)

// TimeLayout is the sentAt format: RFC 3339, UTC, whole seconds.
const TimeLayout = "2006-01-02T15:04:05Z"

// Header is the envelope header. Every member is always present on the
// wire; InReplyTo and BookingRef are null when unset. The signing key and
// algorithm are not here: they are the JWS protected header's.
type Header struct {
	ULPVersion    string  `json:"ulpVersion"`
	Intent        string  `json:"intent"`
	MessageID     string  `json:"messageId"`
	CorrelationID string  `json:"correlationId"`
	InReplyTo     *string `json:"inReplyTo"`
	SentAt        string  `json:"sentAt"`
	Sender        string  `json:"sender"`
	Receiver      string  `json:"receiver"`
	BookingRef    *string `json:"bookingRef"`
}

// NewHeader starts a conversation: a fresh messageId, a correlationId equal
// to it, sentAt from now, no inReplyTo and no bookingRef. Set BookingRef
// for intents about an existing booking.
func NewHeader(intent, sender, receiver string, now time.Time) Header {
	id := newUUID()
	return Header{
		ULPVersion:    Version,
		Intent:        intent,
		MessageID:     id,
		CorrelationID: id,
		SentAt:        now.UTC().Format(TimeLayout),
		Sender:        sender,
		Receiver:      receiver,
	}
}

// Reply builds the header of a response to h: sender and receiver swapped,
// the same correlationId and bookingRef, inReplyTo set to h's messageId, and
// a fresh messageId.
func (h Header) Reply(intent string, now time.Time) Header {
	r := NewHeader(intent, h.Receiver, h.Sender, now)
	r.CorrelationID = h.CorrelationID
	inReplyTo := h.MessageID
	r.InReplyTo = &inReplyTo
	if h.BookingRef != nil {
		ref := *h.BookingRef
		r.BookingRef = &ref
	}
	return r
}

// SentTime parses SentAt.
func (h Header) SentTime() (time.Time, error) {
	t, err := parseSentAt(h.SentAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	return t, nil
}

var (
	// intentPattern: "ulp." and dot-separated lowercase segments, e.g.
	// "ulp.tnt.inquiry.response". Which intents exist is the intents
	// package's business; this only bounds what a header may carry.
	intentPattern = regexp.MustCompile(`^ulp(\.[a-z][a-z0-9_]*)+$`)
	// bookingRefPattern: booking references end up in logs and in model
	// prompts, so they stay short and inert.
	bookingRefPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// Validate checks every header field against its rule. Every error wraps
// ErrMalformed.
//
// Rules that depend on the intent (which intents exist, which need a
// bookingRef) and on the receiving node (receiver is this node, sentAt is
// within the allowed skew, messageId is not a replay) are checked elsewhere.
func (h Header) Validate() error {
	if err := h.validate(); err != nil {
		return fmt.Errorf("%w: header.%w", ErrMalformed, err)
	}
	return nil
}

func (h Header) validate() error {
	if h.ULPVersion != Version {
		return fmt.Errorf("ulpVersion: %s is not %q", logsafe.Quote(h.ULPVersion), Version)
	}
	if len(h.Intent) > 64 || !intentPattern.MatchString(h.Intent) {
		return fmt.Errorf("intent: %s is not a ULP intent name", logsafe.Quote(h.Intent))
	}
	if !isUUIDv4(h.MessageID) {
		return fmt.Errorf("messageId: %s is not a lowercase UUIDv4", logsafe.Quote(h.MessageID))
	}
	if !isUUIDv4(h.CorrelationID) {
		return fmt.Errorf("correlationId: %s is not a lowercase UUIDv4", logsafe.Quote(h.CorrelationID))
	}
	if h.InReplyTo != nil && !isUUIDv4(*h.InReplyTo) {
		return fmt.Errorf("inReplyTo: %s is not a lowercase UUIDv4", logsafe.Quote(*h.InReplyTo))
	}
	if _, err := parseSentAt(h.SentAt); err != nil {
		return err
	}
	if !isDomain(h.Sender) {
		return fmt.Errorf("sender: %s is not a lowercase DNS domain", logsafe.Quote(h.Sender))
	}
	if !isDomain(h.Receiver) {
		return fmt.Errorf("receiver: %s is not a lowercase DNS domain", logsafe.Quote(h.Receiver))
	}
	if h.BookingRef != nil && !bookingRefPattern.MatchString(*h.BookingRef) {
		return fmt.Errorf("bookingRef: %s is not 1-64 characters of [A-Za-z0-9._-]", logsafe.Quote(*h.BookingRef))
	}
	return nil
}

func parseSentAt(s string) (time.Time, error) {
	t, err := time.Parse(TimeLayout, s)
	// The round trip rejects what time.Parse tolerates, such as a
	// single-digit hour: there is one way to write each instant.
	if err != nil || t.Format(TimeLayout) != s {
		return time.Time{}, fmt.Errorf("sentAt: %s is not RFC 3339 UTC in whole seconds (%s)", logsafe.Quote(s), TimeLayout)
	}
	return t, nil
}

// newUUID returns a random (version 4) UUID in canonical lowercase form.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// isUUIDv4 accepts the canonical lowercase form of a version 4, RFC 9562
// variant UUID, and nothing else.
func isUUIDv4(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return false
			}
		}
	}
	return s[14] == '4' && strings.IndexByte("89ab", s[19]) >= 0
}

// ValidDomain reports whether s is well formed as a party identity: the
// rule sender and receiver follow (see isDomain). Form only; where the name
// resolves is the fetcher's to check.
func ValidDomain(s string) bool { return isDomain(s) }

// isDomain accepts a lowercase DNS name of at least two labels, each 1-63
// characters of [a-z0-9-] not starting or ending with '-', 253 characters
// at most, with no trailing dot, and a top-level label that starts with a
// letter. It keeps ports, userinfo, IP literals (including inet_aton forms)
// and uppercase out of the URL host that key discovery builds from the
// sender, and that is all: names like metadata.google.internal, localhost.lan
// or *.nip.io pass.
// The discovery fetch itself must refuse loopback, private, link-local and
// metadata addresses after resolving, and must not follow redirects.
func isDomain(s string) bool {
	if len(s) == 0 || len(s) > 253 {
		return false
	}
	labels := strings.Split(s, ".")
	if len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if len(l) == 0 || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for i := 0; i < len(l); i++ {
			c := l[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	// The top-level label starts with a letter, as every real TLD does. That
	// rules out "10.0.0.1", and also "127.0x1" or "0x7f.0.0.0x1", which the
	// libc resolver (inet_aton) reads as IPv4 addresses.
	last := labels[len(labels)-1]
	return last[0] >= 'a' && last[0] <= 'z'
}
