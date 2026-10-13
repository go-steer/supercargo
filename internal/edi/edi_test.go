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
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestRoundTrip: what Format writes, Parse reads back, separators and
// release characters included.
func TestRoundTrip(t *testing.T) {
	msg := Message{Ref: "42", Type: "IFTSTA", Version: "D", Release: "04A", Agency: "UN", Segments: []Segment{
		Seg("BGM", E("23"), E("A+B:C'D?E"), E("9")),
		Seg("FTX", E("AAA"), nil, nil, E("café, 100% ? done", "second")),
		Seg("LOC", E("175"), E("NLRTM", "139", "6")),
	}}
	at := time.Date(2026, 10, 21, 14, 50, 0, 0, time.UTC)
	data := Write("a.example", "b.example", "CTRL1", at, msg)
	if !strings.Contains(string(data), "A?+B?:C?'D??E") {
		t.Fatalf("separators weren't released:\n%s", data)
	}
	ic, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v\n%s", err, data)
	}
	if ic.Sender != "a.example" || ic.Recipient != "b.example" || ic.Control != "CTRL1" || len(ic.Messages) != 1 {
		t.Fatalf("envelope: %+v", ic)
	}
	got := ic.Messages[0]
	if got.Ref != "42" || got.Type != "IFTSTA" || got.Version != "D" || got.Release != "04A" || got.Agency != "UN" {
		t.Errorf("header: %+v", got)
	}
	want := msg.Segments
	want[1].Elements[1], want[1].Elements[2] = E(""), E("") // empty elements read back as one empty component
	if !reflect.DeepEqual(got.Segments, want) {
		t.Errorf("segments:\n got %q\nwant %q", got.Segments, want)
	}
	if again := DefaultSeparators.Format(append([]Segment{}, got.all()...)); !strings.Contains(string(data), string(again)) {
		t.Errorf("re-formatting differs:\n%s\nvs\n%s", again, data)
	}
}

// TestParseForms: what a valid interchange may look like.
func TestParseForms(t *testing.T) {
	for name, in := range map[string]string{
		"no UNA, no UNB":   "UNH+1+APERAK:D:04A:UN'BGM+305+X+29'UNT+3+1'",
		"one line":         "UNA:+.? 'UNB+UNOW:4+a+b+261021:1450+C'UNH+1+X'UNT+2+1'UNZ+1+C'",
		"CRLF and tabs":    "UNH+1+X'\r\n\tBGM+1'\r\n  UNT+3+1'\r\n",
		"other separators": "UNA|*.# ~UNH*1*X|D~FTX*AAA***a#*b#|c##~UNT*3*1~",
		"two messages":     "UNB+UNOW:4+a+b+x+C'UNH+1+X'UNT+2+1'UNH+2+X'UNT+2+2'UNZ+2+C'",
	} {
		ic, err := Parse([]byte(in))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if name == "other separators" {
			if v := ic.Messages[0].Segments[0].Value(3, 0); v != "a*b|c#" {
				t.Errorf("%s: value %q", name, v)
			}
			if ic.Messages[0].Version != "D" {
				t.Errorf("%s: version %q", name, ic.Messages[0].Version)
			}
		}
	}
}

// TestParseRefuses: every syntax rejection path.
func TestParseRefuses(t *testing.T) {
	ok := "UNH+1+X'BGM+1'UNT+3+1'"
	for name, in := range map[string]string{
		"empty":                    "",
		"oversized":                ok + strings.Repeat(" ", MaxInterchange),
		"not UTF-8":                "UNH+1+X'FTX+\xff'UNT+3+1'",
		"UNA cut short":            "UNA:+.",
		"UNA with a letter":        "UNA:A.? 'UNH+1+X'UNT+2+1'",
		"UNA with repeats":         "UNA::.? 'UNH+1+X'UNT+2+1'",
		"UNA with a space":         "UNA:+.?  UNH+1+X UNT+2+1 ",
		"UNA repetition a letter":  "UNA:+.?A'UNH+1+X'UNT+2+1'",
		"UNA repetition repeated":  "UNA:+.?+'UNH+1+X'UNT+2+1'",
		"unterminated segment":     "UNH+1+X'UNT+2+1",
		"release at the end":       "UNH+1+X'BGM+1?",
		"release of a letter":      "UNH+1+X'BGM+?a'UNT+3+1'",
		"control character":        "UNH+1+X'BGM+a\x01b'UNT+3+1'",
		"newline inside a segment": "UNH+1+X'BGM+a\nb'UNT+3+1'",
		"bad tag":                  "UNH+1+X'bgm+1'UNT+3+1'",
		"tag with components":      "UNH+1+X'BGM:1+1'UNT+3+1'",
		"empty segment":            "UNH+1+X''UNT+3+1'",
		"value too long":           "UNH+1+X'FTX+" + strings.Repeat("a", MaxValue+1) + "'UNT+3+1'",
		"too many elements":        "UNH+1+X'FTX" + strings.Repeat("+a", MaxElements+1) + "'UNT+3+1'",
		"too many components":      "UNH+1+X'FTX+" + strings.Repeat("a:", MaxComponents+1) + "'UNT+3+1'",
		"too many segments":        "UNH+1+X'" + strings.Repeat("FTX'", MaxSegments) + "UNT+3+1'",
		"segment before UNH":       "BGM+1'UNH+1+X'UNT+2+1'",
		"UNH without type":         "UNH+1'UNT+2+1'",
		"UNH without reference":    "UNH++X'UNT+2+1'",
		"no UNT":                   "UNH+1+X'BGM+1'",
		"UNH inside a message":     "UNH+1+X'UNH+2+X'UNT+2+2'",
		"wrong segment count":      "UNH+1+X'BGM+1'UNT+2+1'",
		"count not a number":       "UNH+1+X'UNT+two+1'",
		"mismatched UNT reference": "UNH+1+X'BGM+1'UNT+3+2'",
		"UNB without control":      "UNB+UNOW:4+a+b+x'UNH+1+X'UNT+2+1'UNZ+1'",
		"UNB without UNZ":          "UNB+UNOW:4+a+b+x+C'UNH+1+X'UNT+2+1'",
		"wrong message count":      "UNB+UNOW:4+a+b+x+C'UNH+1+X'UNT+2+1'UNZ+2+C'",
		"mismatched UNZ reference": "UNB+UNOW:4+a+b+x+C'UNH+1+X'UNT+2+1'UNZ+1+D'",
		"UNZ without UNB":          ok + "UNZ+1+C'",
		"segments after UNZ":       "UNB+UNOW:4+a+b+x+C'UNH+1+X'UNT+2+1'UNZ+1+C'BGM+1'",
		"no message":               "UNB+UNOW:4+a+b+x+C'UNZ+0+C'",
	} {
		if _, err := Parse([]byte(in)); !errors.Is(err, ErrSyntax) {
			t.Errorf("%s: %v, want ErrSyntax", name, err)
		}
	}
}

func TestValue(t *testing.T) {
	s := Seg("LOC", E("175"), E("NLRTM", "139"))
	if s.Value(1, 1) != "139" || s.Value(1, 2) != "" || s.Value(2, 0) != "" || s.Value(-1, 0) != "" || s.Value(0, -1) != "" {
		t.Error("Value")
	}
}

func TestContainerNumbers(t *testing.T) {
	for n, want := range map[string]bool{
		"OCBU0000004": true, "OCBU1234563": true, "OCBU1234565": false, "CSQU3054383": true, "MSKU9070323": true,
		"OCBU0000005": false, "OCBU000004": false, "OCB10000004": false, "ocbu0000004": false, "OCBU000000A": false,
	} {
		if validContainer(n) != want {
			t.Errorf("%s: want %v", n, want)
		}
	}
}

func TestParseTime(t *testing.T) {
	for v, want := range map[[2]string]time.Time{
		{"202610211431", "203"}:   time.Date(2026, 10, 21, 14, 31, 0, 0, time.UTC),
		{"20261021143105", "204"}: time.Date(2026, 10, 21, 14, 31, 5, 0, time.UTC),
		{"20261021", "102"}:       time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC),
	} {
		if got, err := parseTime(v[0], v[1]); err != nil || !got.Equal(want) {
			t.Errorf("%v: %v %v", v, got, err)
		}
	}
	for _, v := range [][2]string{{"202610211431", "102"}, {"202613211431", "203"}, {"2026102114", "203"}, {"202610211431", "303"}, {"", "203"}} {
		if _, err := parseTime(v[0], v[1]); !errors.Is(err, ErrContent) {
			t.Errorf("%v: %v, want ErrContent", v, err)
		}
	}
}
