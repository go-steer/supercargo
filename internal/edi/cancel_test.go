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
	"testing"
	"time"
)

var forwarderEnv = Envelope{Sender: "meridian-forwarding.example", Recipient: "polder-haulage.example", Control: "MF000032",
	At: time.Date(2026, 10, 20, 7, 15, 0, 0, time.UTC)}

// TestIFTMINCancel: a cancellation is BGM function 1 with its reference,
// and nothing else; it reads back as one.
func TestIFTMINCancel(t *testing.T) {
	data, err := WriteIFTMIN(Instruction{Reference: "MF-0007-L1", Cancel: true}, forwarderEnv)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "BGM+610+MF-0007-L1+1'") || strings.Contains(string(data), "LOC+") {
		t.Errorf("written: %s", data)
	}
	i, sender, err := ParseIFTMIN(data, nil)
	if err != nil || !i.Cancel || i.Reference != "MF-0007-L1" || sender != forwarderEnv.Sender {
		t.Fatalf("read: %+v, %v", i, err)
	}
	full := instruction()
	full.Cancel = true
	if _, err := WriteIFTMIN(full, forwarderEnv); err == nil {
		t.Error("a cancellation with a trip in it written")
	}
	if _, err := WriteIFTMIN(Instruction{Reference: "-", Cancel: true}, forwarderEnv); err == nil {
		t.Error("a cancellation of no reference written")
	}
	// A cancellation that carries a trip, or an unknown function, isn't read.
	base := "LOC+9+NLTLB:139:6'LOC+7+NLRTM:139:6'" + iftminParties + iftminLine
	for name, data := range map[string]string{
		"a cancellation with a trip": strings.Replace(string(iftmin(base)), "BGM+610+R+9", "BGM+610+R+1", 1),
		"function 5":                 strings.Replace(string(iftmin(base)), "BGM+610+R+9", "BGM+610+R+5", 1),
		"no function":                strings.Replace(string(iftmin(base)), "BGM+610+R+9", "BGM+610+R", 1),
	} {
		if _, _, err := ParseIFTMIN([]byte(data), nil); err == nil {
			t.Errorf("%s: read", name)
		}
	}
}

// TestAPERAKAnswers: an acknowledgement names the interchange it answers,
// once and in form.
func TestAPERAKAnswers(t *testing.T) {
	a := Ack{Reference: "MF-0007-L1", CarrierRef: "PH4471", Accepted: true, Answers: "MF000032"}
	data, err := WriteAPERAK(a, carrierEnv)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ParseAPERAK(data); err != nil || got.Answers != "MF000032" {
		t.Errorf("read: %+v, %v", got, err)
	}
	if _, err := WriteAPERAK(Ack{Reference: "MF-0007-L1", CarrierRef: "PH4471", Accepted: true, Answers: "not a control"}, carrierEnv); err == nil {
		t.Error("a malformed interchange written")
	}
	unt := regexp.MustCompile(`UNT\+([0-9]+)\+`)
	for name, edit := range map[string]func(string) string{
		"twice": func(s string) string {
			s = strings.Replace(s, "RFF+AGO:MF000032'", "RFF+AGO:MF000032'RFF+AGO:MF000033'", 1)
			n, _ := strconv.Atoi(unt.FindStringSubmatch(s)[1]) // UNT counts the segments
			return unt.ReplaceAllString(s, "UNT+"+strconv.Itoa(n+1)+"+")
		},
		"malformed": func(s string) string { return strings.Replace(s, "RFF+AGO:MF000032'", "RFF+AGO:a-b c'", 1) },
	} {
		bad := edit(string(data))
		if _, err := ParseAPERAK([]byte(bad)); err == nil || !strings.Contains(err.Error(), "interchange") {
			t.Errorf("%s: %v, reading %s", name, err, bad)
		}
	}
}
