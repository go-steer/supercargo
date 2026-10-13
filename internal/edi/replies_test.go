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
	"os"
	"reflect"
	"testing"
	"time"
)

var carrierEnv = Envelope{Sender: "polder-haulage.example", Recipient: "meridian-forwarding.example", Control: "PH000117",
	At: time.Date(2026, 10, 20, 7, 15, 0, 0, time.UTC)}

// TestAPERAKRoundTrip: what WriteAPERAK writes, ParseAPERAK reads back.
func TestAPERAKRoundTrip(t *testing.T) {
	for _, a := range []Ack{
		{Reference: "MF-0007-L1", CarrierRef: "PH4471", Accepted: true, TransportDocuments: []string{"ECMR-NL-2026-004471"}},
		{Reference: "MF-0007-L1", CarrierRef: "PH4471", Errors: []string{"12", "Z1"}},
	} {
		data, err := WriteAPERAK(a, carrierEnv)
		if err != nil {
			t.Fatal(err)
		}
		got, err := ParseAPERAK(data)
		if err != nil {
			t.Fatalf("%s: %v", data, err)
		}
		if got.Reference != a.Reference || got.CarrierRef != a.CarrierRef || got.Accepted != a.Accepted ||
			!reflect.DeepEqual(got.TransportDocuments, a.TransportDocuments) || len(got.Errors) != len(a.Errors) || got.Sender != carrierEnv.Sender {
			t.Errorf("round trip %+v → %+v", a, got)
		}
	}
	for name, a := range map[string]Ack{
		"no reference":           {CarrierRef: "PH4471", Accepted: true},
		"no carrier reference":   {Reference: "MF-0007-L1", Accepted: true},
		"errors on an accept":    {Reference: "MF-0007-L1", CarrierRef: "PH4471", Accepted: true, Errors: []string{"12"}},
		"a malformed document":   {Reference: "MF-0007-L1", CarrierRef: "PH4471", Accepted: true, TransportDocuments: []string{"-x"}},
		"a malformed error code": {Reference: "MF-0007-L1", CarrierRef: "PH4471", Errors: []string{"toolong"}},
		"too many documents":     {Reference: "MF-0007-L1", CarrierRef: "PH4471", Accepted: true, TransportDocuments: make([]string, MaxDocuments+1)},
	} {
		if _, err := WriteAPERAK(a, carrierEnv); err == nil {
			t.Errorf("%s: written", name)
		}
	}
	if _, err := WriteAPERAK(Ack{Reference: "R", CarrierRef: "C", Accepted: true}, Envelope{Sender: "a", Recipient: "b", Control: "1"}); err == nil {
		t.Error("an envelope without a time")
	}
	if _, err := WriteAPERAK(Ack{Reference: "R", CarrierRef: "C", Accepted: true}, Envelope{Sender: "a b", Recipient: "b", Control: "1", At: time.Now()}); err == nil {
		t.Error("a malformed sender")
	}
}

// TestIFTSTARoundTrip: what WriteIFTSTA writes, ParseIFTSTAIn reads back,
// in the zone agreed.
func TestIFTSTARoundTrip(t *testing.T) {
	ams, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Skip("no time zone data")
	}
	sts := []Status{
		{Reference: "MF-0007-L1", CarrierRef: "PH4471", Code: "12", Time: time.Date(2026, 10, 21, 5, 45, 0, 0, time.UTC),
			UNLocode: "NLTLB", Facility: "Tilburg Heuvel distribution centre", TransportDocument: "ECMR-NL-2026-004471", Equipment: "OCBU0000004"},
		{Reference: "MF-0007-L1", Code: "21", Time: time.Date(2026, 10, 21, 9, 30, 0, 0, time.UTC), UNLocode: "NLRTM"},
	}
	env := carrierEnv
	env.Location = ams
	data, err := WriteIFTSTA(sts, env)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseIFTSTAIn(data, ams)
	if err != nil {
		t.Fatalf("%s: %v", data, err)
	}
	if len(got) != 2 || !got[0].Time.Equal(sts[0].Time) || got[0].Meaning != PickedUp || got[0].Facility != sts[0].Facility ||
		got[0].Equipment != "OCBU0000004" || got[0].TransportDocument != sts[0].TransportDocument || got[1].Meaning != Delivered || got[1].UNLocode != "NLRTM" {
		t.Errorf("round trip %+v", got)
	}
	for name, edit := range map[string]func(*Status){
		"no reference":            func(s *Status) { s.Reference = "" },
		"a malformed carrier ref": func(s *Status) { s.CarrierRef = "-x" },
		"a malformed code":        func(s *Status) { s.Code = "toolong" },
		"no time":                 func(s *Status) { s.Time = time.Time{} },
		"a bad place":             func(s *Status) { s.UNLocode = "Tilburg" },
		"a bad container":         func(s *Status) { s.Equipment = "OCBU0000005" },
		"a bad document":          func(s *Status) { s.TransportDocument = "-x" },
		"a control in the name":   func(s *Status) { s.Facility = "dock\u0007" },
	} {
		s := sts[0]
		edit(&s)
		if _, err := WriteIFTSTA([]Status{s}, carrierEnv); err == nil {
			t.Errorf("%s: written", name)
		}
	}
	if _, err := WriteIFTSTA(nil, carrierEnv); err == nil {
		t.Error("no statuses")
	}
	if _, err := WriteIFTSTA(sts, Envelope{}); err == nil {
		t.Error("no envelope")
	}
}

// TestParseIFTMIN: the instruction in the test data, and what WriteIFTMIN
// writes, read back; a malformed one refused.
func TestParseIFTMIN(t *testing.T) {
	data, err := os.ReadFile("testdata/iftmin.edi")
	if err != nil {
		t.Fatal(err)
	}
	i, sender, err := ParseIFTMIN(data, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sender != "meridian-forwarding.example" || i.Reference != "MF-0007-L1" || i.ShipperRef != "CYM-PO-88213" || i.Carrier != "POLH" ||
		i.Pickup.UNLocode != "NLTLB" || i.Pickup.Name != "Tilburg Heuvel distribution centre" || i.Delivery.PostalCode != "3199 LD" ||
		!i.Pickup.Latest.Equal(time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)) || !i.Delivery.Latest.Equal(time.Date(2026, 10, 21, 16, 0, 0, 0, time.UTC)) ||
		len(i.Containers) != 1 || i.Containers[0].Count != 1 || i.Containers[0].GrossWeightKg != 12000.5 || i.HSCode != "6404" ||
		i.CargoDescription != "Footwear (sneakers): 2,400 pairs + display units" {
		t.Errorf("instruction %+v", i)
	}
	out, err := WriteIFTMIN(i, Envelope{Sender: "meridian-forwarding.example", Recipient: "polder-haulage.example", Control: "MF000031", At: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if again, _, err := ParseIFTMIN(out, nil); err != nil || !reflect.DeepEqual(again, i) {
		t.Errorf("round trip: %v\n%+v\n%+v", err, again, i)
	}
	for name, bad := range map[string]string{
		"not an IFTMIN":    "UNH+1+IFTSTA:D:04A:UN'BGM+23+X+9'UNT+3+1'",
		"syntax":           "UNH+1",
		"a bad weight":     "UNH+1+IFTMIN:D:04A:UN'RFF+CU:R'EQD+CN++45G1'MEA+AAE+G+KGM:heavy'UNT+5+1'",
		"a bad time":       "UNH+1+IFTMIN:D:04A:UN'RFF+CU:R'LOC+9+NLTLB:139:6'DTM+234:2026:203'UNT+5+1'",
		"a bad container":  "UNH+1+IFTMIN:D:04A:UN'RFF+CU:R'EQD+CN++45G1'EQN+x'UNT+5+1'",
		"nothing required": "UNH+1+IFTMIN:D:04A:UN'RFF+CU:R'UNT+3+1'",
	} {
		if _, _, err := ParseIFTMIN([]byte(bad), nil); err == nil {
			t.Errorf("%s: read", name)
		}
	}
}
