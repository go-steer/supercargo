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

// Package edi speaks the small part of UN/EDIFACT a forwarder needs with
// a carrier that has no ULP node (docs/proposals/door-to-door-forwarder.md):
// a strict syntax layer, an IFTMIN (transport instruction) writer, and
// APERAK (acknowledgement) and IFTSTA (status) parsers, plus a file drop to
// exchange them through.
//
// EDI never enters ULP. Nothing here is signed or verified: a node that
// turns an IFTSTA into an event re-issues it under its own signature and
// marks the source unsigned (derived_from). The syntax layer is strict all
// the same, because the files come from outside: sizes are capped, and
// anything malformed is refused rather than guessed at.
//
// The message subsets are this demo's: faithful to EDIFACT's D.04A
// directories in shape (segments, qualifiers, code lists where it matters),
// not a certified implementation guide. A real partner's guide decides its
// qualifiers and status codes; status codes are mapped through one table
// (MeaningOf) for that reason.
package edi

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Limits on what is parsed.
const (
	MaxInterchange = 1 << 20 // bytes
	MaxSegments    = 10000
	MaxElements    = 64  // per segment
	MaxComponents  = 32  // per element
	MaxValue       = 512 // bytes per component
)

// ErrSyntax reports input that isn't well-formed EDIFACT, or breaks a
// limit.
var ErrSyntax = errors.New("edi: syntax")

// ErrContent reports a well-formed message whose content breaks this
// package's rules (a missing reference, a bad code, a bad date).
var ErrContent = errors.New("edi: content")

func syntaxErr(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrSyntax, fmt.Sprintf(format, a...))
}

func contentErr(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrContent, fmt.Sprintf(format, a...))
}

// Separators are an interchange's service characters (UNA).
type Separators struct {
	Component, Element, Decimal, Release, Repetition, Segment byte
}

// DefaultSeparators are EDIFACT's defaults, used when there is no UNA.
var DefaultSeparators = Separators{Component: ':', Element: '+', Decimal: '.', Release: '?', Repetition: ' ', Segment: '\''}

// special reports whether c must be released (escaped) in a value.
func (s Separators) special(c byte) bool {
	return c == s.Component || c == s.Element || c == s.Release || c == s.Segment || (s.Repetition != ' ' && c == s.Repetition)
}

func (s Separators) valid() error {
	set := map[byte]bool{}
	for _, c := range []byte{s.Component, s.Element, s.Release, s.Segment} {
		if set[c] || c == ' ' || c < 0x20 || c > 0x7e || (c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') {
			return syntaxErr("UNA: separators must be distinct printable punctuation, got %q", []byte{s.Component, s.Element, s.Release, s.Segment})
		}
		set[c] = true
	}
	if s.Decimal != '.' && s.Decimal != ',' {
		return syntaxErr("UNA: decimal mark %q is not . or ,", s.Decimal)
	}
	if r := s.Repetition; r != ' ' && (set[r] || r < 0x21 || r > 0x7e || (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')) {
		return syntaxErr("UNA: repetition separator %q must be a space or other punctuation", r)
	}
	return nil
}

// Segment is one segment: its tag and its elements, each a list of
// components (a simple element has one).
type Segment struct {
	Tag      string
	Elements [][]string
}

// Seg builds a segment.
func Seg(tag string, elements ...[]string) Segment { return Segment{Tag: tag, Elements: elements} }

// E builds an element from its components.
func E(components ...string) []string { return components }

// Value is component c of element e (both from 0; element 0 is the first
// after the tag), or "" if it isn't there.
func (s Segment) Value(e, c int) string {
	if e < 0 || e >= len(s.Elements) || c < 0 || c >= len(s.Elements[e]) {
		return ""
	}
	return s.Elements[e][c]
}

// Message is one message, UNH to UNT: its reference, its type and version
// (IFTSTA, D, 04A, UN), and the segments between.
type Message struct {
	Ref, Type, Version, Release, Agency string
	Segments                            []Segment
}

// Interchange is a parsed interchange: its separators, its envelope (UNB;
// empty without one), and its messages.
type Interchange struct {
	Separators                 Separators
	Sender, Recipient, Control string
	Messages                   []Message
}

var tagPattern = regexp.MustCompile(`^[A-Z][A-Z0-9]{2}$`)

// Parse parses an interchange: an optional UNA, an optional UNB/UNZ
// envelope, and one or more UNH/UNT messages, nothing else. Whitespace
// between segments is ignored; inside a value, C0 controls and DEL are
// not allowed. Other characters a person or model shouldn't be shown
// (C1, bidirectional and other format characters) are refused by the
// message parsers in the fields they return, not here.
func Parse(data []byte) (*Interchange, error) {
	if len(data) > MaxInterchange {
		return nil, syntaxErr("interchange is %d bytes, over %d", len(data), MaxInterchange)
	}
	if !utf8.Valid(data) {
		return nil, syntaxErr("interchange is not valid UTF-8")
	}
	sep := DefaultSeparators
	rest := data
	if strings.HasPrefix(string(rest), "UNA") {
		if len(rest) < 9 {
			return nil, syntaxErr("UNA is cut short")
		}
		sep = Separators{Component: rest[3], Element: rest[4], Decimal: rest[5], Release: rest[6], Repetition: rest[7], Segment: rest[8]}
		if err := sep.valid(); err != nil {
			return nil, err
		}
		rest = rest[9:]
	}
	segs, err := sep.split(rest)
	if err != nil {
		return nil, err
	}
	return structure(sep, segs)
}

// split cuts data into segments, undoing release characters.
func (s Separators) split(data []byte) ([]Segment, error) {
	var segs []Segment
	i := 0
	for {
		for i < len(data) && (data[i] == '\r' || data[i] == '\n' || data[i] == ' ' || data[i] == '\t') {
			i++
		}
		if i >= len(data) {
			return segs, nil
		}
		if len(segs) >= MaxSegments {
			return nil, syntaxErr("more than %d segments", MaxSegments)
		}
		start := i
		var elems [][]string
		var comps []string
		var val []byte
		terminated := false
		for i < len(data) && !terminated {
			c := data[i]
			switch {
			case c == s.Release:
				if i+1 >= len(data) || !s.special(data[i+1]) {
					return nil, syntaxErr("release character at offset %d doesn't release a separator", i)
				}
				val = append(val, data[i+1])
				i++
			case c == s.Component || c == s.Element || c == s.Segment:
				comps = append(comps, string(val))
				val = nil
				if c != s.Component {
					elems = append(elems, comps)
					comps = nil
				}
				terminated = c == s.Segment
			case c < 0x20 || c == 0x7f:
				return nil, syntaxErr("control character at offset %d", i)
			default:
				val = append(val, c)
			}
			i++
			if len(val) > MaxValue {
				return nil, syntaxErr("a value at offset %d is over %d bytes", start, MaxValue)
			}
			if len(comps) > MaxComponents || len(elems) > MaxElements+1 {
				return nil, syntaxErr("segment at offset %d has too many elements or components", start)
			}
		}
		if !terminated {
			return nil, syntaxErr("unterminated segment at offset %d", start)
		}
		if len(elems[0]) != 1 || !tagPattern.MatchString(elems[0][0]) {
			return nil, syntaxErr("segment at offset %d has no valid tag", start)
		}
		segs = append(segs, Segment{Tag: elems[0][0], Elements: elems[1:]})
	}
}

func structure(sep Separators, segs []Segment) (*Interchange, error) {
	ic := &Interchange{Separators: sep}
	i := 0
	if i < len(segs) && segs[i].Tag == "UNB" {
		unb := segs[i]
		ic.Sender, ic.Recipient, ic.Control = unb.Value(1, 0), unb.Value(2, 0), unb.Value(4, 0)
		if !partyPattern.MatchString(ic.Sender) || !partyPattern.MatchString(ic.Recipient) || !controlPattern.MatchString(ic.Control) {
			return nil, syntaxErr("UNB needs a sender, a recipient and a control reference, each well-formed")
		}
		i++
	}
	refs := map[string]bool{}
	for i < len(segs) && segs[i].Tag != "UNZ" {
		unh := segs[i]
		if unh.Tag != "UNH" {
			return nil, syntaxErr("segment %s outside a message", unh.Tag)
		}
		m := Message{Ref: unh.Value(0, 0), Type: unh.Value(1, 0), Version: unh.Value(1, 1), Release: unh.Value(1, 2), Agency: unh.Value(1, 3)}
		if m.Ref == "" || m.Type == "" {
			return nil, syntaxErr("UNH needs a reference and a message type")
		}
		if !controlPattern.MatchString(m.Ref) {
			return nil, syntaxErr("message reference %q is malformed", m.Ref)
		}
		if refs[m.Ref] {
			return nil, syntaxErr("two messages are %s", m.Ref)
		}
		refs[m.Ref] = true
		j := i + 1
		for j < len(segs) && segs[j].Tag != "UNT" {
			if t := segs[j].Tag; t == "UNH" || t == "UNB" || t == "UNZ" || t == "UNA" {
				return nil, syntaxErr("message %s has no UNT before %s", m.Ref, t)
			}
			j++
		}
		if j == len(segs) {
			return nil, syntaxErr("message %s has no UNT", m.Ref)
		}
		unt := segs[j]
		if n, ok := count(unt.Value(0, 0)); !ok || n != j-i+1 {
			return nil, syntaxErr("message %s: UNT counts %q segments, it has %d", m.Ref, unt.Value(0, 0), j-i+1)
		}
		if unt.Value(1, 0) != m.Ref {
			return nil, syntaxErr("message %s ends with UNT for %q", m.Ref, unt.Value(1, 0))
		}
		m.Segments = segs[i+1 : j]
		ic.Messages = append(ic.Messages, m)
		i = j + 1
	}
	switch {
	case ic.Control != "":
		if i >= len(segs) {
			return nil, syntaxErr("interchange %s has no UNZ", ic.Control)
		}
		unz := segs[i]
		if n, ok := count(unz.Value(0, 0)); !ok || n != len(ic.Messages) {
			return nil, syntaxErr("UNZ counts %q messages, there are %d", unz.Value(0, 0), len(ic.Messages))
		}
		if unz.Value(1, 0) != ic.Control {
			return nil, syntaxErr("UNZ closes %q, UNB opened %q", unz.Value(1, 0), ic.Control)
		}
		i++
	case i < len(segs):
		return nil, syntaxErr("UNZ without UNB")
	}
	if i != len(segs) {
		return nil, syntaxErr("segments after the end of the interchange")
	}
	if len(ic.Messages) == 0 {
		return nil, syntaxErr("no message")
	}
	return ic, nil
}

// count reads a UNT or UNZ count: digits only, no sign.
func count(v string) (int, bool) {
	if !countPattern.MatchString(v) {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	return n, err == nil
}

// escape releases every separator in v.
func (s Separators) escape(v string) string {
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		if s.special(v[i]) {
			b.WriteByte(s.Release)
		}
		b.WriteByte(v[i])
	}
	return b.String()
}

// Format writes segments, one per line, releasing separators in values
// and dropping trailing empty components and elements.
func (s Separators) Format(segs []Segment) []byte {
	var b strings.Builder
	for _, seg := range segs {
		b.WriteString(seg.Tag)
		elems := seg.Elements
		for len(elems) > 0 && empty(elems[len(elems)-1]) {
			elems = elems[:len(elems)-1]
		}
		for _, el := range elems {
			for len(el) > 0 && el[len(el)-1] == "" {
				el = el[:len(el)-1]
			}
			b.WriteByte(s.Element)
			for k, c := range el {
				if k > 0 {
					b.WriteByte(s.Component)
				}
				b.WriteString(s.escape(c))
			}
		}
		b.WriteByte(s.Segment)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

func empty(el []string) bool {
	for _, c := range el {
		if c != "" {
			return false
		}
	}
	return true
}

// Segments are the message with its UNH and UNT.
func (m Message) all() []Segment {
	out := []Segment{Seg("UNH", E(m.Ref), E(m.Type, m.Version, m.Release, m.Agency))}
	out = append(out, m.Segments...)
	return append(out, Seg("UNT", E(strconv.Itoa(len(m.Segments)+2)), E(m.Ref)))
}

// Write writes an interchange with the default separators: a UNB/UNZ
// envelope (syntax UNOW, UTF-8) around msgs, prepared at at, written in
// at's own location (pass at.In(loc) for another).
func Write(sender, recipient, control string, at time.Time, msgs ...Message) []byte {
	segs := []Segment{Seg("UNB", E("UNOW", "4"), E(sender), E(recipient), E(at.Format("060102"), at.Format("1504")), E(control))}
	for _, m := range msgs {
		segs = append(segs, m.all()...)
	}
	segs = append(segs, Seg("UNZ", E(strconv.Itoa(len(msgs))), E(control)))
	return DefaultSeparators.Format(segs)
}

// Patterns shared by the messages.
var (
	refPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	locodePattern = regexp.MustCompile(`^[A-Z]{2}[A-Z2-9]{3}$`)
	docPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9/_.-]{0,63}$`)
	partyPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.:_-]{0,34}$`) // UNB sender and recipient, an..35
	// controlPattern is an interchange's or a message's reference, an..14.
	controlPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,13}$`)
	countPattern   = regexp.MustCompile(`^[0-9]{1,6}$`)
)

// checkText checks a free-text value: present if required, at most max
// characters, valid UTF-8, and made only of letters, marks, digits,
// punctuation, symbols and spaces (Unicode L, M, N, P, S and Zs). That
// leaves out what could hide or reorder what a person or a model is
// shown: C0 and C1 controls, DEL, format characters (the bidirectional
// controls, zero-width characters, the BOM, soft hyphens and tag
// characters among them), private-use and unassigned code points,
// noncharacters, variation selectors (which can carry hidden bytes),
// letters and symbols that show as blank (the Hangul fillers, the blank
// braille pattern), runs of more than maxMarks nonspacing marks (which
// can draw over other text), and line and paragraph separators. A script
// that needs a zero-width joiner, a variation selector or long stacks of
// marks can't be written; EDIFACT text rarely is.
func checkText(field, v string, required bool, max int) error {
	if required && v == "" {
		return contentErr("%s is missing", field)
	}
	if !utf8.ValidString(v) {
		return contentErr("%s is not valid UTF-8", field)
	}
	if utf8.RuneCountInString(v) > max {
		return contentErr("%s is over %d characters", field, max)
	}
	marks := 0
	for _, r := range v {
		if !unicode.In(r, unicode.L, unicode.M, unicode.N, unicode.P, unicode.S, unicode.Zs) ||
			unicode.In(r, unicode.Noncharacter_Code_Point, unicode.Variation_Selector) || invisible[r] {
			return contentErr("%s has a control, format, invisible or unassigned character (%U)", field, r)
		}
		if unicode.Is(unicode.Mn, r) {
			marks++
			if marks > maxMarks {
				return contentErr("%s has more than %d combining marks in a row", field, maxMarks)
			}
		} else {
			marks = 0
		}
	}
	return nil
}

// validContainer reports whether n is an ISO 6346 container number with
// a correct check digit.
func validContainer(n string) bool {
	if len(n) != 11 {
		return false
	}
	sum := 0
	for i := range 10 {
		c := n[i]
		var v int
		switch {
		case i == 3 && c != 'U' && c != 'J' && c != 'Z':
			return false // the category: freight container, detachable equipment, trailer or chassis
		case i < 4 && c >= 'A' && c <= 'Z':
			// A=10, skipping multiples of 11.
			v = int(c-'A') + 10
			v += (v - 1) / 10
		case i >= 4 && c >= '0' && c <= '9':
			v = int(c - '0')
		default:
			return false
		}
		sum += v << i
	}
	d := sum % 11 % 10
	return n[10] == byte('0'+d)
}

// maxMarks is the most nonspacing marks checkText allows in a row.
const maxMarks = 4

// invisible are letters and symbols that show as blank.
var invisible = map[rune]bool{0x115F: true, 0x1160: true, 0x3164: true, 0xFFA0: true, 0x2800: true}

// Years a DTM time may fall in.
const (
	minYear = 2000
	maxYear = 2099
)

// parseTime reads a DTM value that must carry a time: format 203
// (CCYYMMDDHHMM) or 204 (CCYYMMDDHHMMSS), which have no zone and are read
// in loc, the zone agreed with the partner (nil is UTC); or 303
// (CCYYMMDDHHMMZZZ) or 304 (CCYYMMDDHHMMSSZZZ), whose ZZZ is the offset
// from UTC in hours, sign first, -12 to +14 (+01, -03; the + released as
// ?+ with the default separators), this package's reading of the zone.
// Format 102, a date without a time, is refused, and so is a year
// outside 2000 to 2099, and a local time loc skips or repeats (as a
// daylight-saving change does, by however much), which only 303 or 304 can
// say unambiguously. The time is returned in UTC.
func parseTime(v, format string, loc *time.Location) (time.Time, error) {
	if format == "102" {
		return time.Time{}, contentErr("date %q is format 102, a date without a time", v)
	}
	layout, ok := map[string]string{"203": "200601021504", "204": "20060102150405", "303": "200601021504", "304": "20060102150405"}[format]
	if !ok {
		return time.Time{}, contentErr("date format %q is not 203, 204, 303 or 304", format)
	}
	if format == "303" || format == "304" {
		if len(v) < 3 {
			return time.Time{}, contentErr("date %q has no zone (ZZZ, +01 or -03)", v)
		}
		zone := v[len(v)-3:]
		v = v[:len(v)-3]
		if !zonePattern.MatchString(zone) {
			return time.Time{}, contentErr("date %q has no zone (ZZZ, +01 or -03)", v+zone)
		}
		h, _ := strconv.Atoi(zone[1:]) // two digits
		if zone[0] == '-' {
			h = -h
		}
		if h < -12 || h > 14 {
			return time.Time{}, contentErr("zone %q is not -12 to +14", zone)
		}
		loc = time.FixedZone(zone, h*3600)
	}
	if loc == nil {
		loc = time.UTC
	}
	t, err := time.ParseInLocation(layout, v, loc)
	if err != nil {
		return time.Time{}, contentErr("date %q is not format %s", v, format)
	}
	if t.Year() < minYear || t.Year() > maxYear {
		return time.Time{}, contentErr("date %q is not in %d to %d", v, minYear, maxYear)
	}
	if t.Format(layout) != v {
		return time.Time{}, contentErr("time %q doesn't exist in %s", v, loc)
	}
	// A change of offset near t, by however much, may repeat t's local
	// time: look for it on the other side of the change.
	_, before := t.Add(-3 * time.Hour).Zone()
	_, after := t.Add(3 * time.Hour).Zone()
	if shift := time.Duration(before-after) * time.Second; shift != 0 {
		for _, d := range []time.Duration{shift, -shift} {
			if t.Add(d).In(loc).Format(layout) == v {
				return time.Time{}, contentErr("time %q happens twice in %s", v, loc)
			}
		}
	}
	return t.UTC(), nil
}

var zonePattern = regexp.MustCompile(`^[+-][0-9]{2}$`)
