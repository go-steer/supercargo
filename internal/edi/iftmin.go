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
	"reflect"
	"regexp"
	"strconv"
	"time"
)

// Instruction is a transport instruction for one leg: what IFTMIN says.
type Instruction struct {
	Reference        string // the forwarder's reference for the leg (RFF+CU)
	ShipperRef       string // optional: the shipper's own reference (RFF+CR)
	Pickup, Delivery Stop
	Containers       []Container
	CargoDescription string
	HSCode           string // optional
	Carrier          string // optional: the carrier's code (TDT)
	// Cancel withdraws the instruction sent under Reference (BGM message
	// function 1): a cancellation names nothing else.
	Cancel bool
}

// Stop is a place the truck calls at, and its window.
type Stop struct {
	Name, Street, Locality, PostalCode, Country, UNLocode string
	Earliest, Latest                                      time.Time // optional
}

// Container is one equipment line: how many of a size type, each weighing.
type Container struct {
	SizeType      string // ISO 6346 size and type, 45G1
	Count         int
	GrossWeightKg float64 // each
}

// Envelope is an interchange's UNB: who sends, to whom, its control
// reference, and when it was prepared; and the zone agreed with the
// partner for times, which format 203 writes without one.
type Envelope struct {
	Sender, Recipient, Control string
	At                         time.Time
	Location                   *time.Location // nil is UTC
}

// Limits on an instruction.
const (
	MaxContainerLines = 20
	MaxContainerCount = 999
	MaxWeightKg       = 100000
)

var (
	countryPattern  = regexp.MustCompile(`^[A-Z]{2}$`)
	sizeTypePattern = regexp.MustCompile(`^[0-9A-Z]{4}$`)
	hsPattern       = regexp.MustCompile(`^[0-9]{4,10}$`)
	carrierPattern  = regexp.MustCompile(`^[A-Z0-9]{2,8}$`)
)

// WriteIFTMIN writes an instruction as an IFTMIN in an interchange of its
// own. The subset (D.04A in shape, the demo's choice of qualifiers):
//
//	UNH+1+IFTMIN:D:04A:UN               message header
//	BGM+610+<reference>+9               transport instruction, original
//	DTM+137:<at>:203                    prepared
//	TSR+30                              door-to-door carriage (this subset's value)
//	FTX+AAA+++<description>             goods description
//	RFF+CU:<reference>                  the forwarder's (consignor's) reference
//	RFF+CR:<shipper ref>                optional: the customer's reference
//	TDT+30++3++<carrier>:172            road on-carriage, by carrier code
//	NAD+CZ / NAD+CN                     consignor and consignee places (pickup, delivery)
//	LOC+9+<locode>:139:6                pickup (place of loading), then
//	DTM+234 / DTM+235                   collection window, earliest and latest
//	LOC+7+<locode>:139:6                delivery, then
//	DTM+64 / DTM+63                     delivery window, earliest and latest
//	GID+1+<count>:CN                    the goods: containers in all
//	PIA+5+<hs>:HS                       optional: HS code
//	MEA+AAE+G+KGM:<kg>                  gross weight in all
//	EQD+CN++<size type>  EQN+<count>  MEA+AAE+G+KGM:<kg>   per equipment line, each
//	UNT+<n>+1
//
// A cancellation (Cancel) is BGM+610+<reference>+1, its DTM+137 and its
// RFF+CU, and nothing else: it withdraws the instruction sent under that
// reference.
//
// Times, the UNB's included, are written in format 203, which has no
// zone, in env.Location, the zone agreed with the partner (UTC if nil).
func WriteIFTMIN(i Instruction, env Envelope) ([]byte, error) {
	if err := i.validate(); err != nil {
		return nil, err
	}
	if !partyPattern.MatchString(env.Sender) || !partyPattern.MatchString(env.Recipient) || !controlPattern.MatchString(env.Control) {
		return nil, contentErr("the envelope needs a sender, a recipient and a control reference of at most 14 characters")
	}
	if env.At.IsZero() {
		return nil, contentErr("the envelope needs a time")
	}
	loc := env.Location
	if loc == nil {
		loc = time.UTC
	}
	for _, t := range []time.Time{env.At, i.Pickup.Earliest, i.Pickup.Latest, i.Delivery.Earliest, i.Delivery.Latest} {
		if !t.IsZero() && !Writable(t, loc) {
			return nil, contentErr("time %s can't be written in %s and read back (a daylight-saving change, or out of range)", t.UTC().Format(time.RFC3339), loc)
		}
	}
	stamp := func(t time.Time) string { return t.In(loc).Format("200601021504") }
	if i.Cancel {
		msg := Message{Ref: "1", Type: "IFTMIN", Version: "D", Release: "04A", Agency: "UN", Segments: []Segment{
			Seg("BGM", E("610"), E(i.Reference), E("1")),
			Seg("DTM", E("137", stamp(env.At), "203")),
			Seg("RFF", E("CU", i.Reference)),
		}}
		return Write(env.Sender, env.Recipient, env.Control, env.At.In(loc), msg), nil
	}
	segs := []Segment{
		Seg("BGM", E("610"), E(i.Reference), E("9")),
		Seg("DTM", E("137", stamp(env.At), "203")),
		Seg("TSR", E("30")),
		Seg("FTX", E("AAA"), nil, nil, E(i.CargoDescription)),
		Seg("RFF", E("CU", i.Reference)),
	}
	if i.ShipperRef != "" {
		segs = append(segs, Seg("RFF", E("CR", i.ShipperRef)))
	}
	tdt := Seg("TDT", E("30"), nil, E("3"))
	if i.Carrier != "" {
		tdt.Elements = append(tdt.Elements, nil, E(i.Carrier, "172"))
	}
	segs = append(segs, tdt, i.Pickup.nad("CZ"), i.Delivery.nad("CN"))
	segs = append(segs, i.Pickup.loc("9", "234", "235", stamp)...)
	segs = append(segs, i.Delivery.loc("7", "64", "63", stamp)...)
	count, total := 0, 0.0
	for _, c := range i.Containers {
		count += c.Count
		total += float64(c.Count) * c.GrossWeightKg
	}
	segs = append(segs, Seg("GID", E("1"), E(strconv.Itoa(count), "CN")))
	if i.HSCode != "" {
		segs = append(segs, Seg("PIA", E("5"), E(i.HSCode, "HS")))
	}
	segs = append(segs, mea(total))
	for _, c := range i.Containers {
		segs = append(segs, Seg("EQD", E("CN"), nil, E(c.SizeType)), Seg("EQN", E(strconv.Itoa(c.Count))), mea(c.GrossWeightKg))
	}
	msg := Message{Ref: "1", Type: "IFTMIN", Version: "D", Release: "04A", Agency: "UN", Segments: segs}
	return Write(env.Sender, env.Recipient, env.Control, env.At.In(loc), msg), nil
}

func mea(kg float64) Segment {
	return Seg("MEA", E("AAE"), E("G"), E("KGM", strconv.FormatFloat(kg, 'f', -1, 64)))
}

func (s Stop) nad(qualifier string) Segment {
	return Seg("NAD", E(qualifier), nil, nil, E(s.Name), E(s.Street), E(s.Locality), nil, E(s.PostalCode), E(s.Country))
}

func (s Stop) loc(qualifier, earliest, latest string, stamp func(time.Time) string) []Segment {
	out := []Segment{Seg("LOC", E(qualifier), E(s.UNLocode, "139", "6"))}
	if !s.Earliest.IsZero() {
		out = append(out, Seg("DTM", E(earliest, stamp(s.Earliest), "203")))
	}
	if !s.Latest.IsZero() {
		out = append(out, Seg("DTM", E(latest, stamp(s.Latest), "203")))
	}
	return out
}

func (i Instruction) validate() error {
	if !refPattern.MatchString(i.Reference) {
		return contentErr("reference %q is not a reference", i.Reference)
	}
	if i.Cancel {
		if !reflect.DeepEqual(i, Instruction{Reference: i.Reference, Cancel: true}) {
			return contentErr("a cancellation names the instruction's reference and nothing else")
		}
		return nil
	}
	if i.ShipperRef != "" && !refPattern.MatchString(i.ShipperRef) {
		return contentErr("shipper reference %q is not a reference", i.ShipperRef)
	}
	for name, s := range map[string]Stop{"pickup": i.Pickup, "delivery": i.Delivery} {
		if err := s.validate(name); err != nil {
			return err
		}
	}
	if i.Pickup.UNLocode == i.Delivery.UNLocode && i.Pickup.Name == i.Delivery.Name {
		return contentErr("pickup and delivery are the same place")
	}
	if len(i.Containers) == 0 || len(i.Containers) > MaxContainerLines {
		return contentErr("an instruction has 1 to %d container lines, not %d", MaxContainerLines, len(i.Containers))
	}
	for _, c := range i.Containers {
		if !sizeTypePattern.MatchString(c.SizeType) {
			return contentErr("container size type %q is not ISO 6346", c.SizeType)
		}
		if c.Count < 1 || c.Count > MaxContainerCount {
			return contentErr("container count %d is not 1 to %d", c.Count, MaxContainerCount)
		}
		if !(c.GrossWeightKg > 0 && c.GrossWeightKg <= MaxWeightKg) {
			return contentErr("gross weight %v kg is not above 0 and at most %d", c.GrossWeightKg, MaxWeightKg)
		}
	}
	if err := text("cargo description", i.CargoDescription, true); err != nil {
		return err
	}
	if i.HSCode != "" && !hsPattern.MatchString(i.HSCode) {
		return contentErr("HS code %q is not 4 to 10 digits", i.HSCode)
	}
	if i.Carrier != "" && !carrierPattern.MatchString(i.Carrier) {
		return contentErr("carrier code %q is not 2 to 8 capitals or digits", i.Carrier)
	}
	return nil
}

func (s Stop) validate(name string) error {
	if err := text(name+" name", s.Name, true); err != nil {
		return err
	}
	for field, v := range map[string]string{"street": s.Street, "locality": s.Locality, "postal code": s.PostalCode} {
		if err := text(name+" "+field, v, false); err != nil {
			return err
		}
	}
	if !countryPattern.MatchString(s.Country) {
		return contentErr("%s country %q is not ISO 3166 alpha-2", name, s.Country)
	}
	if !locodePattern.MatchString(s.UNLocode) {
		return contentErr("%s UN/LOCODE %q is malformed", name, s.UNLocode)
	}
	if !s.Earliest.IsZero() && !s.Latest.IsZero() && s.Latest.Before(s.Earliest) {
		return contentErr("%s window ends before it starts", name)
	}
	return nil
}

// text checks a name or short text: checkText's rules, at most 70
// characters (EDIFACT's an..70 for names and text).
func text(field, v string, required bool) error { return checkText(field, v, required, 70) }
