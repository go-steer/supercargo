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

package edi

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The carrier's side: what a road carrier without a node writes back
// (APERAK, IFTSTA) and reads (IFTMIN). The demo's mock carrier
// (cmd/ulpmock) uses them; each writes what this package's parser reads,
// and checks what it writes as the parser would.

// WriteAPERAK writes an acknowledgement in an interchange of its own:
// accepted (29) with the documents issued, or not accepted (27) with its
// errors (a code each, with a text).
func WriteAPERAK(a Ack, env Envelope) ([]byte, error) {
	if err := checkEnvelope(env); err != nil {
		return nil, err
	}
	if !refPattern.MatchString(a.Reference) || !refPattern.MatchString(a.CarrierRef) {
		return nil, contentErr("an acknowledgement names the instruction's reference and the carrier's own")
	}
	if a.Accepted && len(a.Errors) > 0 {
		return nil, contentErr("an acceptance carries no errors")
	}
	if len(a.TransportDocuments) > MaxDocuments || len(a.Errors) > MaxErrors {
		return nil, contentErr("at most %d documents and %d errors", MaxDocuments, MaxErrors)
	}
	function := "27"
	if a.Accepted {
		function = "29"
	}
	st := newStamper(env)
	segs := []Segment{
		Seg("BGM", E("305"), E(a.CarrierRef), E(function)),
		Seg("DTM", E("137", st.stamp(env.At), "203")),
		Seg("RFF", E("ACW", a.Reference)),
	}
	seen := map[string]bool{}
	for _, d := range a.TransportDocuments {
		if !docPattern.MatchString(d) || seen[d] {
			return nil, contentErr("document number %q is not a document number, or is named twice", d)
		}
		seen[d] = true
		segs = append(segs, Seg("RFF", E("AAS", d)))
	}
	for _, e := range a.Errors {
		if !codePattern.MatchString(e) {
			return nil, contentErr("error code %q is malformed", e)
		}
		segs = append(segs, Seg("ERC", E(e)))
	}
	if st.err != nil {
		return nil, st.err
	}
	msg := Message{Ref: "1", Type: "APERAK", Version: "D", Release: "04A", Agency: "UN", Segments: segs}
	return Write(env.Sender, env.Recipient, env.Control, env.At.In(location(env)), msg), nil
}

// WriteIFTSTA writes statuses in one IFTSTA, a consignment (CNI) for
// each: its reference, document, status code, time, place and container.
func WriteIFTSTA(sts []Status, env Envelope) ([]byte, error) {
	if err := checkEnvelope(env); err != nil {
		return nil, err
	}
	if len(sts) == 0 {
		return nil, contentErr("an IFTSTA has at least one status")
	}
	st := newStamper(env)
	segs := []Segment{
		Seg("BGM", E("23"), E(env.Control), E("9")),
		Seg("DTM", E("137", st.stamp(env.At), "203")),
	}
	for n, s := range sts {
		switch st := s; {
		case !refPattern.MatchString(st.Reference):
			return nil, contentErr("status reference %q is not a reference", st.Reference)
		case st.CarrierRef != "" && !refPattern.MatchString(st.CarrierRef):
			return nil, contentErr("consignment number %q is not a reference", st.CarrierRef)
		case !codePattern.MatchString(st.Code):
			return nil, contentErr("status code %q is malformed", st.Code)
		case st.Time.IsZero():
			return nil, contentErr("status %s has no time", st.Code)
		case st.UNLocode != "" && !locodePattern.MatchString(st.UNLocode):
			return nil, contentErr("place %q is not a UN/LOCODE", st.UNLocode)
		case st.Equipment != "" && !validContainer(st.Equipment):
			return nil, contentErr("container %q is not ISO 6346", st.Equipment)
		case st.TransportDocument != "" && !docPattern.MatchString(st.TransportDocument):
			return nil, contentErr("document number %q is not a document number", st.TransportDocument)
		case st.Facility != "" && st.UNLocode == "":
			return nil, contentErr("status %s names a facility without its place (UN/LOCODE)", st.Code)
		}
		if err := text("facility name", s.Facility, false); err != nil {
			return nil, err
		}
		segs = append(segs, Seg("CNI", E(strconv.Itoa(n+1)), E(s.CarrierRef)), Seg("RFF", E("CU", s.Reference)))
		if s.TransportDocument != "" {
			segs = append(segs, Seg("RFF", E("AAS", s.TransportDocument)))
		}
		segs = append(segs, Seg("STS", E("1"), E(s.Code)), Seg("DTM", E("334", st.stamp(s.Time), "203")))
		if s.UNLocode != "" {
			segs = append(segs, Seg("LOC", E("175"), E(s.UNLocode, "139", "6", s.Facility)))
		}
		if s.Equipment != "" {
			segs = append(segs, Seg("EQD", E("CN"), E(s.Equipment)))
		}
	}
	if st.err != nil {
		return nil, st.err
	}
	msg := Message{Ref: "1", Type: "IFTSTA", Version: "D", Release: "04A", Agency: "UN", Segments: segs}
	return Write(env.Sender, env.Recipient, env.Control, env.At.In(location(env)), msg), nil
}

// ParseIFTMIN reads an instruction from an interchange holding one IFTMIN
// (the subset WriteIFTMIN writes), with times in loc (UTC if nil), and
// checks it as WriteIFTMIN would. Each part is given once (a reference, a
// party, a place, a carrier, a description, an HS code; a count and a
// weight per equipment line), a window's times follow their own place
// (234/235 after LOC+9, 64/63 after LOC+7), and BGM and RFF+CU name the
// same reference; anything else is refused rather than read over. It
// returns the UNB sender too.
func ParseIFTMIN(data []byte, loc *time.Location) (Instruction, string, error) {
	if loc == nil {
		loc = time.UTC
	}
	ic, err := Parse(data)
	if err != nil {
		return Instruction{}, "", err
	}
	if len(ic.Messages) != 1 || ic.Messages[0].Type != "IFTMIN" {
		return Instruction{}, "", contentErr("want one IFTMIN message")
	}
	var i Instruction
	var bgm string
	seen := map[string]bool{}
	once := func(part string) error {
		if seen[part] {
			return contentErr("IFTMIN gives %s twice", part)
		}
		seen[part] = true
		return nil
	}
	var at string // the place the last LOC named: "9" or "7"
	var line *Container
	var lineSeen map[string]bool
	for _, s := range ic.Messages[0].Segments {
		var err error
		switch s.Tag {
		case "BGM":
			if err = once("BGM"); err == nil {
				bgm = s.Value(1, 0)
			}
		case "RFF":
			switch q := s.Value(0, 0); q {
			case "CU", "CR":
				if err = once("RFF+" + q); err == nil && q == "CU" {
					i.Reference = s.Value(0, 1)
				} else if err == nil {
					i.ShipperRef = s.Value(0, 1)
				}
			}
		case "FTX":
			if s.Value(0, 0) == "AAA" {
				if err = once("FTX+AAA"); err == nil {
					i.CargoDescription = s.Value(3, 0)
				}
			}
		case "TDT":
			if err = once("TDT"); err == nil {
				i.Carrier = s.Value(4, 0)
			}
		case "NAD":
			q := s.Value(0, 0)
			stop := map[string]*Stop{"CZ": &i.Pickup, "CN": &i.Delivery}[q]
			if stop == nil {
				break
			}
			if err = once("NAD+" + q); err == nil {
				// The party's name and address; its place and window come
				// from its LOC and DTMs, before or after.
				stop.Name, stop.Street, stop.Locality, stop.PostalCode, stop.Country = s.Value(3, 0), s.Value(4, 0), s.Value(5, 0), s.Value(7, 0), s.Value(8, 0)
			}
		case "LOC":
			q := s.Value(0, 0)
			stop := map[string]*Stop{"9": &i.Pickup, "7": &i.Delivery}[q]
			if stop == nil {
				break
			}
			if err = once("LOC+" + q); err == nil {
				stop.UNLocode, at = s.Value(1, 0), q
			}
		case "DTM":
			q := s.Value(0, 0)
			if q == "137" {
				break // the message's own
			}
			stop, earliest := map[string]*Stop{"234": &i.Pickup, "235": &i.Pickup, "64": &i.Delivery, "63": &i.Delivery}[q], q == "234" || q == "64"
			want := map[string]string{"234": "9", "235": "9", "64": "7", "63": "7"}[q]
			switch {
			case stop == nil:
				err = contentErr("IFTMIN date qualifier %q is not one of the subset's", q)
			case want != at:
				err = contentErr("IFTMIN date %s doesn't follow its place (LOC+%s)", q, want)
			default:
				if err = once("DTM+" + q); err == nil {
					var t time.Time
					if t, err = parseTime(s.Value(0, 1), s.Value(0, 2), loc); err == nil && earliest {
						stop.Earliest = t
					} else if err == nil {
						stop.Latest = t
					}
				}
			}
		case "PIA":
			if err = once("PIA"); err == nil {
				i.HSCode = s.Value(1, 0)
			}
		case "EQD":
			i.Containers = append(i.Containers, Container{SizeType: s.Value(2, 0)})
			line, lineSeen = &i.Containers[len(i.Containers)-1], map[string]bool{}
		case "EQN":
			switch {
			case line == nil:
				err = contentErr("IFTMIN gives a count (EQN) before any equipment (EQD)")
			case lineSeen["EQN"]:
				err = contentErr("IFTMIN gives an equipment line's count twice")
			default:
				lineSeen["EQN"] = true
				n, ok := count(s.Value(0, 0))
				if !ok {
					err = contentErr("container count %q is malformed", s.Value(0, 0))
				}
				line.Count = n
			}
		case "MEA":
			if line == nil {
				break // the goods' total, before the lines: checked by the lines
			}
			if lineSeen["MEA"] {
				err = contentErr("IFTMIN gives an equipment line's weight twice")
				break
			}
			lineSeen["MEA"] = true
			line.GrossWeightKg, err = weight(s.Value(2, 1), ic.Separators.Decimal)
		}
		if err != nil {
			return Instruction{}, "", err
		}
	}
	if bgm != i.Reference {
		return Instruction{}, "", contentErr("BGM names %q, RFF+CU %q", bgm, i.Reference)
	}
	if err := i.validate(); err != nil {
		return Instruction{}, "", err
	}
	return i, ic.Sender, nil
}

var weightPattern = regexp.MustCompile(`^[0-9]{1,6}([.,][0-9]{1,3})?$`)

// weight reads a weight in kilograms: digits, and up to three decimals
// after the interchange's decimal mark (no signs, exponents or hex).
func weight(v string, decimal byte) (float64, error) {
	if !weightPattern.MatchString(v) || (strings.ContainsAny(v, ".,") && !strings.ContainsRune(v, rune(decimal))) {
		return 0, contentErr("weight %q is not kilograms with up to three decimals", v)
	}
	return strconv.ParseFloat(strings.Replace(v, ",", ".", 1), 64)
}

func checkEnvelope(env Envelope) error {
	if !partyPattern.MatchString(env.Sender) || !partyPattern.MatchString(env.Recipient) || !controlPattern.MatchString(env.Control) {
		return contentErr("the envelope needs a sender, a recipient and a control reference of at most 14 characters")
	}
	if env.At.IsZero() {
		return contentErr("the envelope needs a time")
	}
	return nil
}

func location(env Envelope) *time.Location {
	if env.Location == nil {
		return time.UTC
	}
	return env.Location
}

// stamper writes times in format 203 in the envelope's zone, refusing
// one the parser wouldn't read back (a local time a daylight-saving
// change skips or repeats, a year out of range); err holds the first.
type stamper struct {
	loc *time.Location
	err error
}

func newStamper(env Envelope) *stamper { return &stamper{loc: location(env)} }

func (s *stamper) stamp(t time.Time) string {
	v := t.In(s.loc).Format("200601021504")
	if _, err := parseTime(v, "203", s.loc); err != nil && s.err == nil {
		s.err = err
	}
	return v
}

// Writable reports whether t can be written in format 203 in loc and read
// back: not in an hour a daylight-saving change skips or repeats there,
// and in the years the parser accepts.
func Writable(t time.Time, loc *time.Location) bool {
	if loc == nil {
		loc = time.UTC
	}
	_, err := parseTime(t.In(loc).Format("200601021504"), "203", loc)
	return err == nil
}
