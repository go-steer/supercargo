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
	"time"
)

// Meaning is what a status code means, as far as this package knows.
type Meaning string

// The meanings. Unknown is a code the mapping doesn't know: it is kept
// raw in Status.Code, never guessed at.
const (
	Unknown   Meaning = "unknown"
	PickedUp  Meaning = "picked_up"
	Departed  Meaning = "departed"
	Arrived   Meaning = "arrived"
	Delivered Meaning = "delivered"
	GateIn    Meaning = "gate_in"
)

// statusCodes maps STS status codes to meanings. The codes are the demo
// partner guide's: a real carrier's implementation guide decides its own,
// and the mapping changes with it.
var statusCodes = map[string]Meaning{
	"12": PickedUp,
	"13": Departed,
	"14": Arrived,
	"21": Delivered,
	"54": GateIn,
}

// MeaningOf maps a status code; a code it doesn't know is Unknown.
func MeaningOf(code string) Meaning {
	if m, ok := statusCodes[code]; ok {
		return m
	}
	return Unknown
}

// Status is one status of one consignment: what an IFTSTA says, one entry
// per STS.
type Status struct {
	Reference         string    // the forwarder's leg reference (RFF+CU)
	CarrierRef        string    // the carrier's consignment number (CNI), if given
	Code              string    // the STS status code, raw
	Meaning           Meaning   // the code's meaning, or Unknown
	Time              time.Time // when it happened (DTM+334), in UTC
	UNLocode          string    // where it happened (LOC+175), if given
	Facility          string    // the place's name (LOC or NAD), if given
	Equipment         string    // the container (EQD+CN), ISO 6346, if given
	TransportDocument string    // the transport document (RFF+AAS), an e-CMR's number, if given
	Sender            string    // provenance: the UNB sender
	ControlRef        string    // provenance: the control/message references
}

var codePattern = regexp.MustCompile(`^[A-Z0-9]{1,3}$`)

// ParseIFTSTA parses an interchange of IFTSTA messages into statuses, in
// order, reading times without a zone as UTC. It is ParseIFTSTAIn with
// time.UTC.
func ParseIFTSTA(data []byte) ([]Status, error) { return ParseIFTSTAIn(data, time.UTC) }

// ParseIFTSTAIn parses an interchange of IFTSTA messages into statuses,
// in order. The subset:
//
//	CNI+<seq>+<carrier ref>          a consignment; its RFF (CU, AAS) apply to its statuses
//	STS+1+<code>                     a status, in a consignment; then, for it:
//	RFF+CU:<reference>  RFF+AAS:<document>   if the consignment has them, the same
//	DTM+334:<time>:<203|204|303|304> when (required)
//	LOC+175+<locode>:139:6:<name>    where it happened
//	NAD+<qualifier>+++<name>         the facility's name, if LOC has none (one per status, any qualifier)
//	EQD+CN+<container number>        the container
//
// Formats 203 and 204 carry no zone and are read in loc, the zone agreed
// with the partner (nil is UTC); 303 and 304 carry their offset (see
// parseTime). A date without a time (102) is refused. Other segments and
// qualifiers, LOC's place of departure (5) and destination (8) among
// them, are ignored. A status without a reference or a time, an STS
// outside a consignment, a malformed code, time, UN/LOCODE, container
// number or document number, a repeated time, place, facility name or
// container on one status, a reference or document given twice at one level, or one on a
// status that contradicts its consignment's is refused, and so is the
// whole interchange with it.
func ParseIFTSTAIn(data []byte, loc *time.Location) ([]Status, error) {
	if loc == nil {
		loc = time.UTC
	}
	ic, err := Parse(data)
	if err != nil {
		return nil, err
	}
	var out []Status
	for _, m := range ic.Messages {
		if m.Type != "IFTSTA" {
			return nil, contentErr("message %s is %s, not IFTSTA", m.Ref, m.Type)
		}
		got, err := statuses(m, ic.Sender, controlRef(ic, m), loc)
		if err != nil {
			return nil, err
		}
		out = append(out, got...)
	}
	return out, nil
}

// refsSeen records the references one level (a consignment or a status)
// has given itself.
type refsSeen struct{ cu, aas bool }

func statuses(m Message, sender, ref string, loc *time.Location) ([]Status, error) {
	var out []Status
	var consignment, cur *Status
	var consignmentRefs, curRefs refsSeen
	var nadName string
	flush := func() error {
		if cur == nil {
			return nil
		}
		if cur.Facility == "" {
			cur.Facility = nadName
		}
		if !refPattern.MatchString(cur.Reference) {
			return contentErr("message %s: status %s has no reference (RFF+CU)", m.Ref, cur.Code)
		}
		if cur.Time.IsZero() {
			return contentErr("message %s: status %s for %s has no time (DTM+334)", m.Ref, cur.Code, cur.Reference)
		}
		out = append(out, *cur)
		cur, nadName = nil, ""
		return nil
	}
	for _, s := range m.Segments {
		switch s.Tag {
		case "CNI":
			if err := flush(); err != nil {
				return nil, err
			}
			carrierRef := s.Value(1, 0)
			if carrierRef != "" && !refPattern.MatchString(carrierRef) {
				return nil, contentErr("consignment number %q is not a reference", carrierRef)
			}
			consignment = &Status{CarrierRef: carrierRef, Sender: sender, ControlRef: ref}
			consignmentRefs = refsSeen{}
			continue
		case "STS":
			if consignment == nil {
				return nil, contentErr("message %s: STS outside a consignment (CNI)", m.Ref)
			}
			if err := flush(); err != nil {
				return nil, err
			}
			code := s.Value(1, 0)
			if !codePattern.MatchString(code) {
				return nil, contentErr("status code %q is malformed", code)
			}
			st := *consignment
			st.Code, st.Meaning = code, MeaningOf(code)
			cur, curRefs = &st, refsSeen{}
			continue
		}
		if consignment == nil {
			continue // message level: BGM, DTM+137
		}
		if s.Tag == "RFF" {
			var err error
			if cur == nil {
				err = applyRef(consignment, &consignmentRefs, nil, s)
			} else {
				err = applyRef(cur, &curRefs, consignment, s)
			}
			if err != nil {
				return nil, err
			}
		}
		if cur == nil {
			continue // a consignment carries references only
		}
		var err error
		switch s.Tag {
		case "DTM":
			err = applyTime(cur, s, loc)
		case "LOC":
			err = applyLoc(cur, s)
		case "NAD":
			if n := s.Value(3, 0); n != "" {
				if nadName != "" {
					err = contentErr("status %s has two facility names (NAD)", cur.Code)
					break
				}
				err = text("facility name", n, false)
				nadName = n
			}
		case "EQD":
			err = applyEquipment(cur, s)
		}
		if err != nil {
			return nil, err
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return out, nil
}

// applyRef sets a reference (CU) or transport document (AAS) on st, a
// consignment (parent nil) or a status (parent its consignment). Each is
// given at most once per level, and a status's must match its
// consignment's, so no later segment silently overrides an earlier one.
func applyRef(st *Status, seen *refsSeen, parent *Status, s Segment) error {
	v := s.Value(0, 1)
	switch s.Value(0, 0) {
	case "CU":
		if !refPattern.MatchString(v) {
			return contentErr("reference %q (RFF+CU) is not a reference", v)
		}
		if seen.cu {
			return contentErr("two references (RFF+CU) at one level, %q and %q", st.Reference, v)
		}
		if parent != nil && parent.Reference != "" && parent.Reference != v {
			return contentErr("status reference %q (RFF+CU) contradicts its consignment's %q", v, parent.Reference)
		}
		seen.cu, st.Reference = true, v
	case "AAS":
		if !docPattern.MatchString(v) {
			return contentErr("transport document %q is malformed", v)
		}
		if seen.aas {
			return contentErr("two transport documents (RFF+AAS) at one level, %q and %q", st.TransportDocument, v)
		}
		if parent != nil && parent.TransportDocument != "" && parent.TransportDocument != v {
			return contentErr("status transport document %q (RFF+AAS) contradicts its consignment's %q", v, parent.TransportDocument)
		}
		seen.aas, st.TransportDocument = true, v
	}
	return nil
}

// A consignment passes on references only, so a status's time, place and
// container are its own, each set at most once.
func applyTime(st *Status, s Segment, loc *time.Location) error {
	if s.Value(0, 0) != "334" {
		return nil
	}
	if !st.Time.IsZero() {
		return contentErr("status %s has two times", st.Code)
	}
	t, err := parseTime(s.Value(0, 1), s.Value(0, 2), loc)
	if err != nil {
		return err
	}
	st.Time = t
	return nil
}

func applyLoc(st *Status, s Segment) error {
	if s.Value(0, 0) != "175" { // the place of the status itself
		return nil
	}
	if st.UNLocode != "" {
		return contentErr("status %s has two places", st.Code)
	}
	code := s.Value(1, 0)
	if !locodePattern.MatchString(code) {
		return contentErr("UN/LOCODE %q is malformed", code)
	}
	name := s.Value(1, 3)
	if err := text("location name", name, false); err != nil {
		return err
	}
	st.UNLocode, st.Facility = code, name
	return nil
}

func applyEquipment(st *Status, s Segment) error {
	if s.Value(0, 0) != "CN" {
		return nil
	}
	if st.Equipment != "" {
		return contentErr("status %s has two containers", st.Code)
	}
	n := s.Value(1, 0)
	if !validContainer(n) {
		return contentErr("container number %q is not ISO 6346", n)
	}
	st.Equipment = n
	return nil
}
