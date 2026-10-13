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
	"errors"
	"flag"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite testdata/iftmin.edi")

func instruction() Instruction {
	return Instruction{
		Reference:  "MF-0007-L1",
		ShipperRef: "CYM-PO-88213",
		Pickup: Stop{Name: "Tilburg Heuvel distribution centre", Street: "Heuvelstraat 12", Locality: "Tilburg",
			PostalCode: "5038 AA", Country: "NL", UNLocode: "NLTLB",
			Earliest: time.Date(2026, 10, 21, 7, 0, 0, 0, time.UTC), Latest: time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)},
		Delivery: Stop{Name: "ECT Delta Terminal", Street: "Europaweg 875", Locality: "Rotterdam",
			PostalCode: "3199 LD", Country: "NL", UNLocode: "NLRTM",
			Latest: time.Date(2026, 10, 21, 16, 0, 0, 0, time.UTC)},
		Containers:       []Container{{SizeType: "45G1", Count: 1, GrossWeightKg: 12000.5}},
		CargoDescription: "Footwear (sneakers): 2,400 pairs + display units",
		HSCode:           "6404",
		Carrier:          "POLH",
	}
}

var envelope = Envelope{Sender: "meridian-forwarding.example", Recipient: "polder-haulage.example", Control: "MF000031",
	At: time.Date(2026, 10, 20, 9, 2, 0, 0, time.UTC)}

// TestIFTMIN: the instruction as written matches testdata/iftmin.edi
// (go test -run TestIFTMIN -update rewrites it), and reads back.
func TestIFTMIN(t *testing.T) {
	got, err := WriteIFTMIN(instruction(), envelope)
	if err != nil {
		t.Fatal(err)
	}
	if *update {
		if err := os.WriteFile("testdata/iftmin.edi", got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile("testdata/iftmin.edi")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("IFTMIN:\n%s\nwant\n%s", got, want)
	}
	ic, err := Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	m := ic.Messages[0]
	if m.Type != "IFTMIN" || m.Version != "D" || m.Release != "04A" {
		t.Errorf("header %+v", m)
	}
	found := map[string]bool{}
	for _, s := range m.Segments {
		found[s.Tag+"+"+s.Value(0, 0)] = true
		if s.Tag == "FTX" && s.Value(3, 0) != instruction().CargoDescription {
			t.Errorf("description read back as %q", s.Value(3, 0))
		}
	}
	for _, k := range []string{"BGM+610", "RFF+CU", "RFF+CR", "TDT+30", "NAD+CZ", "NAD+CN", "LOC+9", "LOC+7", "DTM+234", "DTM+235", "DTM+63", "GID+1", "PIA+5", "MEA+AAE", "EQD+CN", "EQN+1"} {
		if !found[k] {
			t.Errorf("no %s", k)
		}
	}
	if found["DTM+64"] {
		t.Error("a delivery earliest without one")
	}

	// The optional parts left out.
	i := instruction()
	i.ShipperRef, i.HSCode, i.Carrier = "", "", ""
	i.Containers = append(i.Containers, Container{SizeType: "22G1", Count: 2, GrossWeightKg: 8000})
	got, err = WriteIFTMIN(i, envelope)
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{"RFF+CR", "PIA+", ":172"} {
		if strings.Contains(string(got), absent) {
			t.Errorf("%s written without its field", absent)
		}
	}
	if !strings.Contains(string(got), "GID+1+3:CN'") || !strings.Contains(string(got), "MEA+AAE+G+KGM:28000.5'") {
		t.Errorf("totals:\n%s", got)
	}
}

// TestIFTMINRefuses: every instruction and envelope rejection path.
func TestIFTMINRefuses(t *testing.T) {
	for name, edit := range map[string]func(*Instruction, *Envelope){
		"no reference":           func(i *Instruction, _ *Envelope) { i.Reference = "" },
		"reference with a slash": func(i *Instruction, _ *Envelope) { i.Reference = "MF/1" },
		"bad shipper reference":  func(i *Instruction, _ *Envelope) { i.ShipperRef = "a b" },
		"no pickup name":         func(i *Instruction, _ *Envelope) { i.Pickup.Name = "" },
		"long delivery name":     func(i *Instruction, _ *Envelope) { i.Delivery.Name = strings.Repeat("x", 71) },
		"control in a street":    func(i *Instruction, _ *Envelope) { i.Pickup.Street = "a\nb" },
		"bad country":            func(i *Instruction, _ *Envelope) { i.Pickup.Country = "nl" },
		"bad UN/LOCODE":          func(i *Instruction, _ *Envelope) { i.Delivery.UNLocode = "NLRT1" },
		"inverted window":        func(i *Instruction, _ *Envelope) { i.Pickup.Latest = i.Pickup.Earliest.Add(-time.Hour) },
		"same place":             func(i *Instruction, _ *Envelope) { i.Delivery = i.Pickup },
		"no containers":          func(i *Instruction, _ *Envelope) { i.Containers = nil },
		"too many container lines": func(i *Instruction, _ *Envelope) {
			i.Containers = make([]Container, MaxContainerLines+1)
		},
		"bad size type":       func(i *Instruction, _ *Envelope) { i.Containers[0].SizeType = "45g1" },
		"zero count":          func(i *Instruction, _ *Envelope) { i.Containers[0].Count = 0 },
		"too many":            func(i *Instruction, _ *Envelope) { i.Containers[0].Count = MaxContainerCount + 1 },
		"zero weight":         func(i *Instruction, _ *Envelope) { i.Containers[0].GrossWeightKg = 0 },
		"too heavy":           func(i *Instruction, _ *Envelope) { i.Containers[0].GrossWeightKg = MaxWeightKg + 1 },
		"no description":      func(i *Instruction, _ *Envelope) { i.CargoDescription = "" },
		"bad HS code":         func(i *Instruction, _ *Envelope) { i.HSCode = "64.04" },
		"bad carrier":         func(i *Instruction, _ *Envelope) { i.Carrier = "polh" },
		"no sender":           func(_ *Instruction, e *Envelope) { e.Sender = "" },
		"bad recipient":       func(_ *Instruction, e *Envelope) { e.Recipient = "a+b" },
		"long control":        func(_ *Instruction, e *Envelope) { e.Control = "MF0000000000031" },
		"envelope without at": func(_ *Instruction, e *Envelope) { e.At = time.Time{} },
	} {
		i, e := instruction(), envelope
		edit(&i, &e)
		if _, err := WriteIFTMIN(i, e); !errors.Is(err, ErrContent) {
			t.Errorf("%s: %v, want ErrContent", name, err)
		}
	}
}

func read(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestAPERAK(t *testing.T) {
	ack, err := ParseAPERAK(read(t, "aperak.edi"))
	if err != nil {
		t.Fatal(err)
	}
	want := Ack{Reference: "MF-0007-L1", CarrierRef: "PH4471", Accepted: true, TransportDocuments: []string{"ECMR-NL-2026-004471"},
		Sender: "polder-haulage.example", ControlRef: "PH000117/1"}
	if !reflect.DeepEqual(ack, want) {
		t.Errorf("got %+v\nwant %+v", ack, want)
	}

	rejected := "UNH+9+APERAK:D:04A:UN'BGM+305+PH4472+27'RFF+ACW:MF-0007-L2'ERC+12'FTX+AAO+++Pickup window outside our hours'FTX+AAO+++a second text, ignored'ERC+Z01'FTX+ZZZ+++ignored'UNT+9+9'"
	ack, err = ParseAPERAK([]byte(rejected))
	if err != nil {
		t.Fatal(err)
	}
	if ack.Accepted || ack.ControlRef != "9" || !reflect.DeepEqual(ack.Errors, []string{"12: Pickup window outside our hours", "Z01"}) {
		t.Errorf("rejected: %+v", ack)
	}
}

func TestAPERAKRefuses(t *testing.T) {
	body := "BGM+305+PH4471+29'RFF+ACW:MF-0007-L1'"
	wrap := func(segs string) string {
		return "UNH+1+APERAK:D:04A:UN'" + segs + "UNT+" + strconv.Itoa(strings.Count(segs, "'")+2) + "+1'"
	}
	for name, in := range map[string]string{
		"syntax":              "UNH+1+APERAK'",
		"not an APERAK":       "UNH+1+IFTSTA:D:04A:UN'" + body + "UNT+4+1'",
		"two messages":        "UNB+UNOW:4+a+b+x+C'" + wrap(body) + strings.NewReplacer("UNH+1+", "UNH+2+", "+1'", "+2'").Replace(wrap(body)) + "UNZ+2+C'",
		"no BGM":              wrap("RFF+ACW:MF-0007-L1'"),
		"two BGM":             wrap(body + "BGM+305+PH4471+29'"),
		"unknown function":    wrap("BGM+305+PH4471+9'RFF+ACW:MF-0007-L1'"),
		"no reference":        wrap("BGM+305+PH4471+29'"),
		"bad reference":       wrap("BGM+305+PH4471+29'RFF+ACW:MF 1'"),
		"two references":      wrap(body + "RFF+ACW:MF-0007-L2'"),
		"no carrier ref":      wrap("BGM+305++29'RFF+ACW:MF-0007-L1'"),
		"bad document":        wrap(body + "RFF+AAS:ECMR NL'"),
		"too many documents":  wrap(body + strings.Repeat("RFF+AAS:D1'", MaxDocuments+1)),
		"ERC without code":    wrap("BGM+305+PH4471+27'RFF+ACW:MF-0007-L1'ERC'"),
		"too many errors":     wrap("BGM+305+PH4471+27'RFF+ACW:MF-0007-L1'" + strings.Repeat("ERC+1'", MaxErrors+1)),
		"accepts with errors": wrap(body + "ERC+12'"),
	} {
		_, err := ParseAPERAK([]byte(in))
		if err == nil {
			t.Errorf("%s: accepted", name)
		} else if name != "syntax" && !errors.Is(err, ErrContent) {
			t.Errorf("%s: %v, want ErrContent", name, err)
		}
	}
}

func TestIFTSTA(t *testing.T) {
	got, err := ParseIFTSTA(read(t, "iftsta-picked-up.edi"))
	if err != nil {
		t.Fatal(err)
	}
	base := Status{Reference: "MF-0007-L1", CarrierRef: "PH4471", Equipment: "OCBU0000004", TransportDocument: "ECMR-NL-2026-004471",
		Sender: "polder-haulage.example"}
	want := base
	want.Code, want.Meaning, want.Time, want.UNLocode, want.Facility, want.ControlRef =
		"12", PickedUp, time.Date(2026, 10, 21, 7, 45, 0, 0, time.UTC), "NLTLB", "Tilburg Heuvel distribution centre", "PH000121/1"
	if !reflect.DeepEqual(got, []Status{want}) {
		t.Errorf("picked up:\n got %+v\nwant %+v", got, want)
	}

	got, err = ParseIFTSTA(read(t, "iftsta-delivered.edi"))
	if err != nil {
		t.Fatal(err)
	}
	gateIn, delivered := base, base
	gateIn.Code, gateIn.Meaning, gateIn.Time, gateIn.UNLocode, gateIn.Facility, gateIn.ControlRef =
		"54", GateIn, time.Date(2026, 10, 21, 14, 22, 0, 0, time.UTC), "NLRTM", "ECT Delta: Maasvlakte", "PH000128/7"
	delivered.Code, delivered.Meaning, delivered.Time, delivered.UNLocode, delivered.Facility, delivered.ControlRef =
		"21", Delivered, time.Date(2026, 10, 21, 14, 31, 0, 0, time.UTC), "NLRTM", "ECT Delta Terminal", "PH000128/7"
	if !reflect.DeepEqual(got, []Status{gateIn, delivered}) {
		t.Errorf("delivered:\n got %+v\nwant %+v %+v", got, gateIn, delivered)
	}

	// A code the mapping doesn't know is kept, unmapped; a status's own
	// reference overrides its consignment's; ignored qualifiers are ignored.
	in := "UNH+1+IFTSTA:D:04A:UN'BGM+23+X+9'DTM+137:202610211450:203'CNI+1'RFF+CU:MF-0007-L1'RFF+ZZZ:x'" +
		"STS+1+99'RFF+CU:MF-0007-L9'DTM+178:20261021:102'DTM+334:20261021:102'LOC+11+XXXXX'EQD+BX+whatever'NAD+DP'UNT+14+1'"
	got, err = ParseIFTSTA([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Meaning != Unknown || got[0].Code != "99" || got[0].Reference != "MF-0007-L9" ||
		got[0].CarrierRef != "" || got[0].UNLocode != "" || got[0].Equipment != "" || got[0].Facility != "" || got[0].ControlRef != "1" {
		t.Errorf("unknown code: %+v", got)
	}
	if MeaningOf("13") != Departed || MeaningOf("14") != Arrived {
		t.Error("MeaningOf")
	}
}

func TestIFTSTARefuses(t *testing.T) {
	wrap := func(segs string) string {
		return "UNH+1+IFTSTA:D:04A:UN'" + segs + "UNT+" + strconv.Itoa(strings.Count(segs, "'")+2) + "+1'"
	}
	ok := "CNI+1+PH4471'RFF+CU:MF-0007-L1'STS+1+12'DTM+334:202610210745:203'"
	for name, in := range map[string]string{
		"syntax":                 "UNH+1+IFTSTA'",
		"not an IFTSTA":          "UNH+1+APERAK:D:04A:UN'" + ok + "UNT+6+1'",
		"STS outside CNI":        wrap("STS+1+12'RFF+CU:MF-0007-L1'DTM+334:202610210745:203'"),
		"no reference":           wrap("CNI+1'STS+1+12'DTM+334:202610210745:203'"),
		"bad reference":          wrap("CNI+1'RFF+CU:MF 1'STS+1+12'DTM+334:202610210745:203'"),
		"no time":                wrap("CNI+1'RFF+CU:MF-0007-L1'STS+1+12'"),
		"no time, then another":  wrap("CNI+1'RFF+CU:MF-0007-L1'STS+1+12'STS+1+21'DTM+334:202610210745:203'"),
		"no time, then CNI":      wrap("CNI+1'RFF+CU:MF-0007-L1'STS+1+12'CNI+2'"),
		"bad consignment number": wrap("CNI+1+PH 4471'RFF+CU:MF-0007-L1'STS+1+12'DTM+334:202610210745:203'"),
		"no code":                wrap("CNI+1'RFF+CU:MF-0007-L1'STS+1'DTM+334:202610210745:203'"),
		"bad code":               wrap("CNI+1'RFF+CU:MF-0007-L1'STS+1+pu'DTM+334:202610210745:203'"),
		"bad date":               wrap(ok[:len(ok)-len("DTM+334:202610210745:203'")] + "DTM+334:202610320745:203'"),
		"bad date format":        wrap(ok[:len(ok)-len("DTM+334:202610210745:203'")] + "DTM+334:202610210745:999'"),
		"two times":              wrap(ok + "DTM+334:202610210746:203'"),
		"bad UN/LOCODE":          wrap(ok + "LOC+175+NLTL1'"),
		"two places":             wrap(ok + "LOC+175+NLTLB'LOC+5+NLRTM'"),
		"long LOC name":          wrap(ok + "LOC+175+NLTLB:139:6:" + strings.Repeat("x", 71) + "'"),
		"long NAD name":          wrap(ok + "NAD+DP+++" + strings.Repeat("x", 71) + "'"),
		"bad container":          wrap(ok + "EQD+CN+OCBU0000005'"),
		"two containers":         wrap(ok + "EQD+CN+OCBU0000004'EQD+CN+OCBU1234563'"),
		"bad document":           wrap(ok + "RFF+AAS:ECMR NL'"),
		"bad consignment ref":    wrap("CNI+1'RFF+CU:MF 1'"),
	} {
		_, err := ParseIFTSTA([]byte(in))
		if err == nil {
			t.Errorf("%s: accepted", name)
		} else if name != "syntax" && !errors.Is(err, ErrContent) {
			t.Errorf("%s: %v, want ErrContent", name, err)
		}
	}
}
