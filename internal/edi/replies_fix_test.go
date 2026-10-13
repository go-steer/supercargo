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
	"strings"
	"testing"
	"time"
)

// TestWritersRefuseUnreadableTimes: a time the parser wouldn't read back
// (Amsterdam's repeated hour when the clocks go back on 25 October 2026,
// or a year out of range) isn't written, in any message.
func TestWritersRefuseUnreadableTimes(t *testing.T) {
	ams, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Skip("no time zone data")
	}
	repeated := time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC) // 02:30 in Amsterdam, twice
	if Writable(repeated, ams) || !Writable(repeated, nil) || Writable(time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC), nil) {
		t.Error("Writable")
	}
	env := carrierEnv
	env.Location = ams
	st := Status{Reference: "MF-0007-L1", Code: "12", Time: repeated, UNLocode: "NLTLB"}
	if _, err := WriteIFTSTA([]Status{st}, env); err == nil {
		t.Error("an IFTSTA in the repeated hour")
	}
	late := env
	late.At = repeated
	if _, err := WriteAPERAK(Ack{Reference: "MF-0007-L1", CarrierRef: "PH4471", Accepted: true}, late); err == nil {
		t.Error("an APERAK prepared in the repeated hour")
	}
	i := Instruction{Reference: "MF-0007-L1", CargoDescription: "Footwear",
		Pickup:     Stop{Name: "W", Country: "NL", UNLocode: "NLTLB", Earliest: repeated},
		Delivery:   Stop{Name: "T", Country: "NL", UNLocode: "NLRTM"},
		Containers: []Container{{SizeType: "45G1", Count: 1, GrossWeightKg: 12000}}}
	if _, err := WriteIFTMIN(i, Envelope{Sender: "a", Recipient: "b", Control: "1", At: time.Now(), Location: ams}); err == nil {
		t.Error("an IFTMIN window in the repeated hour")
	}
}

// TestWritersMatchParsers: what a parser refuses, the writer doesn't
// write: a document twice, a facility without its place.
func TestWritersMatchParsers(t *testing.T) {
	if _, err := WriteAPERAK(Ack{Reference: "R", CarrierRef: "C", Accepted: true, TransportDocuments: []string{"D-1", "D-1"}}, carrierEnv); err == nil {
		t.Error("a document twice")
	}
	if _, err := WriteIFTSTA([]Status{{Reference: "R", Code: "12", Time: time.Now(), Facility: "Dock 4"}}, carrierEnv); err == nil {
		t.Error("a facility without its place")
	}
}

const iftminHead = "UNH+1+IFTMIN:D:04A:UN'BGM+610+R+9'RFF+CU:R'FTX+AAA+++Shoes'"
const iftminParties = "NAD+CZ+++Warehouse++Tilburg+++NL'NAD+CN+++Terminal++Rotterdam+++NL'"
const iftminLine = "EQD+CN++45G1'EQN+1'MEA+AAE+G+KGM:12000'"

func iftmin(body string) []byte {
	n := strings.Count(iftminHead+body, "'")
	return []byte(iftminHead + body + "UNT+" + strconv.Itoa(n+1) + "+1'")
}

// TestParseIFTMINStrict: a party after its place keeps the place's
// window; each part is given once; a window's time follows its own
// place; BGM and RFF+CU agree; a weight is plain kilograms, with the
// interchange's decimal mark.
func TestParseIFTMINStrict(t *testing.T) {
	ok := "LOC+9+NLTLB:139:6'DTM+234:202610210700:203'LOC+7+NLRTM:139:6'" + iftminParties + iftminLine
	i, _, err := ParseIFTMIN(iftmin(ok), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !i.Pickup.Earliest.Equal(time.Date(2026, 10, 21, 7, 0, 0, 0, time.UTC)) || i.Pickup.Name != "Warehouse" || i.Pickup.UNLocode != "NLTLB" {
		t.Errorf("a party after its place lost something: %+v", i.Pickup)
	}
	comma := "UNA:+,? '" + string(iftmin("LOC+9+NLTLB:139:6'LOC+7+NLRTM:139:6'"+iftminParties+"EQD+CN++45G1'EQN+1'MEA+AAE+G+KGM:12000,5'"))
	if i, _, err := ParseIFTMIN([]byte(comma), nil); err != nil || i.Containers[0].GrossWeightKg != 12000.5 {
		t.Errorf("a comma decimal mark: %+v, %v", i.Containers, err)
	}
	base := "LOC+9+NLTLB:139:6'LOC+7+NLRTM:139:6'" + iftminParties
	for name, body := range map[string]string{
		"RFF+CU twice":        "RFF+CU:S'" + base + iftminLine,
		"a party twice":       base + "NAD+CZ+++Other++Tilburg+++NL'" + iftminLine,
		"a place twice":       base + "LOC+9+NLTLB:139:6'" + iftminLine,
		"a time twice":        "LOC+9+NLTLB:139:6'DTM+234:202610210700:203'DTM+234:202610210800:203'LOC+7+NLRTM:139:6'" + iftminParties + iftminLine,
		"a time out of place": "LOC+9+NLTLB:139:6'LOC+7+NLRTM:139:6'DTM+234:202610210700:203'" + iftminParties + iftminLine,
		"an unknown time":     "LOC+9+NLTLB:139:6'DTM+999:202610210700:203'LOC+7+NLRTM:139:6'" + iftminParties + iftminLine,
		"a carrier twice":     "TDT+30++3++A:172'TDT+30++3++B:172'" + base + iftminLine,
		"a description twice": "FTX+AAA+++Boots'" + base + iftminLine,
		"an HS code twice":    "PIA+5+6404:HS'PIA+5+6403:HS'" + base + iftminLine,
		"a count first":       "EQN+1'" + base + iftminLine,
		"a count twice":       base + "EQD+CN++45G1'EQN+1'EQN+2'MEA+AAE+G+KGM:12000'",
		"a weight twice":      base + "EQD+CN++45G1'EQN+1'MEA+AAE+G+KGM:12000'MEA+AAE+G+KGM:1'",
		"a hex weight":        base + "EQD+CN++45G1'EQN+1'MEA+AAE+G+KGM:0x1p4'",
		"a comma, undeclared": base + "EQD+CN++45G1'EQN+1'MEA+AAE+G+KGM:12000,5'",
		"BGM twice":           "BGM+610+R+9'" + base + iftminLine,
	} {
		if _, _, err := ParseIFTMIN(iftmin(body), nil); err == nil {
			t.Errorf("%s: read", name)
		}
	}
	other := strings.Replace(string(iftmin(base+iftminLine)), "BGM+610+R+9", "BGM+610+S+9", 1)
	if _, _, err := ParseIFTMIN([]byte(other), nil); err == nil {
		t.Error("BGM and RFF+CU disagree: read")
	}
}
