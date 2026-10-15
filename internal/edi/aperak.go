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

import "slices"

// Ack is a carrier's answer to an instruction: what APERAK says.
type Ack struct {
	Reference          string   // the instruction's reference (RFF+ACW)
	CarrierRef         string   // the carrier's document number (BGM)
	Accepted           bool     // BGM message function 29 (accepted) or 27 (not accepted)
	TransportDocuments []string // documents the carrier issued (RFF+AAS), an e-CMR's number
	Errors             []string // ERC codes, each with its FTX text if any; partner text, checked but untrusted
	// Answers is the control reference of the interchange it answers
	// (RFF+AGO): an instruction's, or its cancellation's. Optional.
	Answers            string
	Sender, ControlRef string // provenance: the UNB sender, and the control/message references
}

// Limits on an acknowledgement.
const (
	MaxDocuments = 8
	MaxErrors    = 20
	MaxErrorText = 256 // characters of an error's FTX text
)

// ParseAPERAK parses an interchange holding one APERAK. The subset:
//
//	BGM+<doc name>+<carrier ref>+<29|27>   accepted, or not accepted
//	RFF+ACW:<reference>                    the instruction it answers
//	RFF+AGO:<control reference>            optional: the interchange it answers (the instruction's or its cancellation's)
//	RFF+AAS:<document number>              documents issued (at most 8)
//	ERC+<code>  FTX+AAO+++<text>           errors (at most 20); only when not accepted
//
// Other segments are ignored. A missing or malformed reference, an
// unknown message function, errors on an acceptance, a malformed error
// code (1 to 3 capitals or digits), and error text over MaxErrorText
// characters or with control or bidirectional-control characters are
// refused.
func ParseAPERAK(data []byte) (Ack, error) {
	ic, err := Parse(data)
	if err != nil {
		return Ack{}, err
	}
	if len(ic.Messages) != 1 || ic.Messages[0].Type != "APERAK" {
		return Ack{}, contentErr("want one APERAK message")
	}
	m := ic.Messages[0]
	ack := Ack{Sender: ic.Sender, ControlRef: controlRef(ic, m)}
	sawBGM, sawACW, sawAGO, explained := false, false, false, false
	for _, s := range m.Segments {
		switch s.Tag {
		case "BGM":
			if sawBGM {
				return Ack{}, contentErr("APERAK has two BGM segments")
			}
			sawBGM = true
			ack.CarrierRef = s.Value(1, 0)
			switch s.Value(2, 0) {
			case "29":
				ack.Accepted = true
			case "27":
			default:
				return Ack{}, contentErr("APERAK message function %q is not 29 (accepted) or 27 (not accepted)", s.Value(2, 0))
			}
		case "RFF":
			switch v := s.Value(0, 1); s.Value(0, 0) {
			case "ACW":
				if sawACW {
					return Ack{}, contentErr("APERAK names two instructions")
				}
				sawACW, ack.Reference = true, v
			case "AGO":
				if sawAGO {
					return Ack{}, contentErr("APERAK names two interchanges it answers")
				}
				if !controlPattern.MatchString(v) {
					return Ack{}, contentErr("the interchange answered, %q (RFF+AGO), is not a control reference", v)
				}
				sawAGO, ack.Answers = true, v
			case "AAS":
				if !docPattern.MatchString(v) {
					return Ack{}, contentErr("transport document %q is malformed", v)
				}
				if slices.Contains(ack.TransportDocuments, v) {
					return Ack{}, contentErr("transport document %q is named twice", v)
				}
				if len(ack.TransportDocuments) == MaxDocuments {
					return Ack{}, contentErr("more than %d transport documents", MaxDocuments)
				}
				ack.TransportDocuments = append(ack.TransportDocuments, v)
			}
		case "ERC":
			if len(ack.Errors) == MaxErrors {
				return Ack{}, contentErr("more than %d errors", MaxErrors)
			}
			if !codePattern.MatchString(s.Value(0, 0)) {
				return Ack{}, contentErr("error code %q (ERC) is malformed", s.Value(0, 0))
			}
			ack.Errors = append(ack.Errors, s.Value(0, 0))
			explained = false
		case "FTX":
			if n := len(ack.Errors); n > 0 && !explained && s.Value(0, 0) == "AAO" && s.Value(3, 0) != "" {
				if err := checkText("error text", s.Value(3, 0), false, MaxErrorText); err != nil {
					return Ack{}, err
				}
				ack.Errors[n-1] += ": " + s.Value(3, 0) // the first text only
				explained = true
			}
		}
	}
	if !sawBGM {
		return Ack{}, contentErr("APERAK has no BGM")
	}
	if !refPattern.MatchString(ack.Reference) {
		return Ack{}, contentErr("APERAK reference %q (RFF+ACW) is not a reference", ack.Reference)
	}
	if !refPattern.MatchString(ack.CarrierRef) {
		return Ack{}, contentErr("APERAK document number %q is not a reference", ack.CarrierRef)
	}
	if ack.Accepted && len(ack.Errors) > 0 {
		return Ack{}, contentErr("APERAK accepts and reports errors")
	}
	return ack, nil
}

// controlRef names a message for provenance: the interchange's control
// reference and the message's, or the message's alone without a UNB.
func controlRef(ic *Interchange, m Message) string {
	if ic.Control == "" {
		return m.Ref
	}
	return ic.Control + "/" + m.Ref
}
