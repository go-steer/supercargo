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
	"strconv"
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
	stamp := stamper(env)
	segs := []Segment{
		Seg("BGM", E("305"), E(a.CarrierRef), E(function)),
		Seg("DTM", E("137", stamp(env.At), "203")),
		Seg("RFF", E("ACW", a.Reference)),
	}
	for _, d := range a.TransportDocuments {
		if !docPattern.MatchString(d) {
			return nil, contentErr("document number %q is not a document number", d)
		}
		segs = append(segs, Seg("RFF", E("AAS", d)))
	}
	for _, e := range a.Errors {
		if !codePattern.MatchString(e) {
			return nil, contentErr("error code %q is malformed", e)
		}
		segs = append(segs, Seg("ERC", E(e)))
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
	stamp := stamper(env)
	segs := []Segment{
		Seg("BGM", E("23"), E(env.Control), E("9")),
		Seg("DTM", E("137", stamp(env.At), "203")),
	}
	for n, st := range sts {
		switch {
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
		}
		if err := text("facility name", st.Facility, false); err != nil {
			return nil, err
		}
		segs = append(segs, Seg("CNI", E(strconv.Itoa(n+1)), E(st.CarrierRef)), Seg("RFF", E("CU", st.Reference)))
		if st.TransportDocument != "" {
			segs = append(segs, Seg("RFF", E("AAS", st.TransportDocument)))
		}
		segs = append(segs, Seg("STS", E("1"), E(st.Code)), Seg("DTM", E("334", stamp(st.Time), "203")))
		if st.UNLocode != "" {
			segs = append(segs, Seg("LOC", E("175"), E(st.UNLocode, "139", "6", st.Facility)))
		}
		if st.Equipment != "" {
			segs = append(segs, Seg("EQD", E("CN"), E(st.Equipment)))
		}
	}
	msg := Message{Ref: "1", Type: "IFTSTA", Version: "D", Release: "04A", Agency: "UN", Segments: segs}
	return Write(env.Sender, env.Recipient, env.Control, env.At.In(location(env)), msg), nil
}

// ParseIFTMIN reads an instruction from an interchange holding one IFTMIN
// (the subset WriteIFTMIN writes), with times in loc (UTC if nil), and
// checks it as WriteIFTMIN would. It returns the UNB sender too.
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
	var at *Stop // the stop the last LOC named, for its DTMs
	var line *Container
	for _, s := range ic.Messages[0].Segments {
		switch s.Tag {
		case "RFF":
			switch s.Value(0, 0) {
			case "CU":
				i.Reference = s.Value(0, 1)
			case "CR":
				i.ShipperRef = s.Value(0, 1)
			}
		case "FTX":
			if s.Value(0, 0) == "AAA" {
				i.CargoDescription = s.Value(3, 0)
			}
		case "TDT":
			i.Carrier = s.Value(4, 0)
		case "NAD":
			st := Stop{Name: s.Value(3, 0), Street: s.Value(4, 0), Locality: s.Value(5, 0), PostalCode: s.Value(7, 0), Country: s.Value(8, 0)}
			switch s.Value(0, 0) {
			case "CZ":
				st.UNLocode = i.Pickup.UNLocode
				i.Pickup = st
			case "CN":
				st.UNLocode = i.Delivery.UNLocode
				i.Delivery = st
			}
		case "LOC":
			switch s.Value(0, 0) {
			case "9":
				i.Pickup.UNLocode, at = s.Value(1, 0), &i.Pickup
			case "7":
				i.Delivery.UNLocode, at = s.Value(1, 0), &i.Delivery
			}
		case "DTM":
			if at == nil {
				continue // the message's own (137)
			}
			t, err := parseTime(s.Value(0, 1), s.Value(0, 2), loc)
			if err != nil {
				return Instruction{}, "", err
			}
			switch s.Value(0, 0) {
			case "234", "64":
				at.Earliest = t
			case "235", "63":
				at.Latest = t
			}
		case "PIA":
			i.HSCode = s.Value(1, 0)
		case "EQD":
			i.Containers = append(i.Containers, Container{SizeType: s.Value(2, 0)})
			line = &i.Containers[len(i.Containers)-1]
		case "EQN":
			if line != nil {
				n, ok := count(s.Value(0, 0))
				if !ok {
					return Instruction{}, "", contentErr("container count %q is malformed", s.Value(0, 0))
				}
				line.Count = n
			}
		case "MEA":
			if line != nil {
				kg, err := strconv.ParseFloat(s.Value(2, 1), 64)
				if err != nil {
					return Instruction{}, "", contentErr("weight %q is malformed", s.Value(2, 1))
				}
				line.GrossWeightKg = kg
			}
		}
	}
	if err := i.validate(); err != nil {
		return Instruction{}, "", err
	}
	return i, ic.Sender, nil
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

// stamper writes times in format 203 in the envelope's zone.
func stamper(env Envelope) func(time.Time) string {
	loc := location(env)
	return func(t time.Time) string { return t.In(loc).Format("200601021504") }
}
