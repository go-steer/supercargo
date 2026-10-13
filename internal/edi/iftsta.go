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
	Time              time.Time // when it happened (DTM+334), UTC
	UNLocode          string    // where (LOC+175, 5 or 8), if given
	Facility          string    // the place's name (LOC or NAD), if given
	Equipment         string    // the container (EQD+CN), ISO 6346, if given
	TransportDocument string    // the transport document (RFF+AAS), an e-CMR's number, if given
	Sender            string    // provenance: the UNB sender
	ControlRef        string    // provenance: the control/message references
}

var codePattern = regexp.MustCompile(`^[A-Z0-9]{1,3}$`)

// ParseIFTSTA parses an interchange of IFTSTA messages into statuses, in
// order. The subset:
//
//	CNI+<seq>+<carrier ref>          a consignment; its RFF (CU, AAS) apply to its statuses
//	STS+1+<code>                     a status, in a consignment; then, for it:
//	RFF+CU:<reference>  RFF+AAS:<document>
//	DTM+334:<time>:<203|204|102>     when (required)
//	LOC+<175|5|8>+<locode>:139:6:<name>   where
//	NAD+<qualifier>+++<name>         the facility's name, if LOC has none
//	EQD+CN+<container number>        the container
//
// Times carry no zone in these formats and are read as UTC, which the
// demo partner guide requires. Other segments and qualifiers are ignored.
// A status without a
// reference or a time, an STS outside a consignment, a malformed code,
// time, UN/LOCODE, container number or document number, or a repeated
// time, place or container on one status is refused, and so is the whole
// interchange with it.
func ParseIFTSTA(data []byte) ([]Status, error) {
	ic, err := Parse(data)
	if err != nil {
		return nil, err
	}
	var out []Status
	for _, m := range ic.Messages {
		if m.Type != "IFTSTA" {
			return nil, contentErr("message %s is %s, not IFTSTA", m.Ref, m.Type)
		}
		got, err := statuses(m, ic.Sender, controlRef(ic, m))
		if err != nil {
			return nil, err
		}
		out = append(out, got...)
	}
	return out, nil
}

func statuses(m Message, sender, ref string) ([]Status, error) {
	var out []Status
	var consignment, cur *Status
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
			cur = &st
			continue
		}
		target := cur
		if target == nil {
			target = consignment
		}
		if target == nil {
			continue // message level: BGM, DTM+137
		}
		if s.Tag == "RFF" {
			if err := applyRef(target, s); err != nil {
				return nil, err
			}
		}
		if cur == nil {
			continue // a consignment carries references only
		}
		var err error
		switch s.Tag {
		case "DTM":
			err = applyTime(cur, s)
		case "LOC":
			err = applyLoc(cur, s)
		case "NAD":
			if n := s.Value(3, 0); n != "" && nadName == "" {
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

func applyRef(st *Status, s Segment) error {
	v := s.Value(0, 1)
	switch s.Value(0, 0) {
	case "CU":
		if !refPattern.MatchString(v) {
			return contentErr("reference %q (RFF+CU) is not a reference", v)
		}
		st.Reference = v
	case "AAS":
		if !docPattern.MatchString(v) {
			return contentErr("transport document %q is malformed", v)
		}
		st.TransportDocument = v
	}
	return nil
}

// A consignment passes on references only, so a status's time, place and
// container are its own, each set at most once.
func applyTime(st *Status, s Segment) error {
	if s.Value(0, 0) != "334" {
		return nil
	}
	if !st.Time.IsZero() {
		return contentErr("status %s has two times", st.Code)
	}
	t, err := parseTime(s.Value(0, 1), s.Value(0, 2))
	if err != nil {
		return err
	}
	st.Time = t
	return nil
}

func applyLoc(st *Status, s Segment) error {
	switch s.Value(0, 0) {
	case "175", "5", "8":
	default:
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
