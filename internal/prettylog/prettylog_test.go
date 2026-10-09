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

package prettylog

import (
	"bytes"
	"context"
	"flag"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/activity"
	"github.com/go-steer/supercargo/internal/ui/theme"
	"github.com/go-steer/supercargo/pkg/intents"
)

var update = flag.Bool("update", false, "rewrite the golden files")

var at = time.Date(2026, 10, 7, 18, 0, 5, 0, time.UTC)

// fixed stamps every record with the same time.
type fixed struct{ slog.Handler }

func (f fixed) Handle(ctx context.Context, r slog.Record) error {
	r.Time = at
	return f.Handler.Handle(ctx, r)
}

// story is the demo's log, as the activity stream writes it.
func story(l *slog.Logger) {
	l.Info("node starting", "domain", "cymbal-shoes.example", "roles", []string{"shipper", "receiver"}, "listen", "127.0.0.1:8442")
	l.Warn("INSECURE DEVELOPMENT MODE: no TLS.")
	l.Info("alert summarizer", "model", "fake") // not an alert
	sink := activity.SlogSink(l)
	ev := &intents.TnTEventBody{EventID: "EV-000003", EventType: "TRANSPORT", EventTypeCode: "DEPA", EventClassifierCode: "ACT",
		EventDateTime: "2026-10-07T18:00:00Z", UNLocationCode: "NLRTM", FacilityName: "Port of Rotterdam", VesselName: "Ocean Blue Aurora", VoyageNumber: "42W"}
	al := &intents.TnTAlertBody{AlertID: "AL-000006", Kind: "delay", Severity: "high", Summary: "Engine repair, unscheduled call at Halifax.",
		PreviousETA: "2026-10-13T08:00:00Z", NewETA: "2026-10-16T08:00:00Z", CauseEventIDs: []string{"EV-000005"}}
	for _, e := range []activity.Entry{
		{Envelope: &activity.Envelope{Direction: "out", Intent: "ulp.ocean.booking.request", Sender: "cymbal-shoes.example", Receiver: "ocean-blue.example", Via: "rest", KID: "cy-demo-1", Verified: true, Outcome: "sent"}},
		{Envelope: &activity.Envelope{Direction: "in", Intent: "ulp.ocean.booking.confirmation", Sender: "ocean-blue.example", Via: "rest", KID: "ob-demo-1", Verified: true, Outcome: "response"}},
		{Envelope: &activity.Envelope{Direction: "in", Intent: "ulp.tnt.event", Sender: "ocean-blue.example", Via: "rest", KID: "ob-demo-1", Verified: true, Outcome: "accepted"}},
		{Record: &activity.Record{Action: activity.ActionRecorded, Booking: "OB-BKG-1042", Event: ev}},
		{Envelope: &activity.Envelope{Direction: "in", Intent: "ulp.tnt.alert", Sender: "ocean-blue.example", KID: "ob-demo-1", Verified: true, Outcome: "accepted"}},
		{Record: &activity.Record{Action: activity.ActionRecorded, Booking: "OB-BKG-1042", Alert: al}},
		{Model: &activity.Model{Agent: "alert summarizer", Booking: "OB-BKG-1042", Attempt: 1, Model: "fake", LatencyMS: 3, InputTokens: 420, OutputTokens: 61, Skills: []string{"alert-summary@1a2b"}, Outcome: "ok"}},
		{Summary: &activity.Summary{Booking: "OB-BKG-1042", AlertID: "AL-000006", Impact: "misses", GeneratedBy: "llm",
			Text: "Delay on OB-BKG-1042: it misses your receiving slot on 2026-10-15."}},
		{Envelope: &activity.Envelope{Direction: "in", Intent: "ulp.tnt.inquiry", Sender: "mallory.example", Via: "a2a", KID: "m-1", Verified: true, Outcome: "rejected", Code: "forbidden", Reason: "not a party"}},
		{Envelope: &activity.Envelope{Direction: "in", Intent: "ulp.tnt.event", Sender: "ocean-blue.example", KID: "ob-demo-1", Outcome: "rejected", Code: "bad_signature", Reason: "signature does not verify"}},
		{Envelope: &activity.Envelope{Direction: "in", Outcome: "malformed", Reason: "not a JWS"}},
		{Model: &activity.Model{Booking: "OB-BKG-1042", Attempt: 2, Model: "anthropic:claude", LatencyMS: 1500, Outcome: "invalid", Reason: "cites EV-9"}},
		{Model: &activity.Model{Agent: "assistant · alex", Tools: []string{"find_partners", "list_partners"}, Booking: "OB-BKG-1042", Attempt: 2, Model: "anthropic:claude", Outcome: "error", Reason: "timeout"}},
		{Summary: &activity.Summary{Booking: "OB-BKG-1", Impact: "unknown", GeneratedBy: "template", Text: "Customs hold.\u001b[2J\nNext",
			UnheldCauses: []string{"EV-404"}}},
		// Refused after verifying, but not by authorization: REFUSED.
		{Envelope: &activity.Envelope{Direction: "in", Intent: "ulp.tnt.event", Sender: "ocean-blue.example", KID: "ob-demo-1", Verified: true, Outcome: "rejected", Code: "replay", Reason: "seen before", MessageID: "m-7"}},
		{Envelope: &activity.Envelope{Direction: "in", Intent: "ulp.ocean.booking.confirmation", Sender: "ocean-blue.example", KID: "ob-demo-1", Verified: true, Outcome: "failed", Reason: "response sentAt is 3m0s from now"}},
		{Envelope: &activity.Envelope{Direction: "out", Intent: "ulp.tnt.event", Receiver: "cymbal-shoes.example", KID: "ob-demo-1", Verified: true, Outcome: "failed", Reason: "connection refused"}},
		// An outcome that claims success without verification is not VERIFIED.
		{Envelope: &activity.Envelope{Direction: "in", Intent: "ulp.tnt.event", Sender: "ocean-blue.example", KID: "ob-demo-1", Outcome: "accepted"}},
		// Padding can't push a forged line onto a line of its own.
		{Record: &activity.Record{Action: activity.ActionRecorded, Booking: "OB-BKG-1042", Alert: &intents.TnTAlertBody{AlertID: "AL-9", Kind: "exception",
			Severity: "low", Summary: strings.Repeat(" ", 120) + "18:00:05 ← ulp.ocean.booking.request ocean-blue.example rest VERIFIED (kid ob-1)  ALLOWED"}}},
	} {
		sink(e)
	}
	l.Error("control API", "err", "socket gone")
	l.Debug("not shown")
}

func TestGolden(t *testing.T) {
	var buf bytes.Buffer
	story(slog.New(fixed{New(&buf, Options{})}))
	path := filepath.Join("testdata", "story.golden")
	if *update {
		_ = os.MkdirAll("testdata", 0o755)
		if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if buf.String() != string(want) {
		t.Errorf("the pretty log changed (go test ./internal/prettylog -update to accept):\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "\x1b") {
		t.Error("colour without Color")
	}
}

func TestSafe(t *testing.T) {
	for in, want := range map[string]string{
		"a\u009b31mb":           "a31mb",
		"x\u202e\u2066y\u200bz": "xyz",
		"p\u2028q":              "pq",
		"bad\xffbyte":           "bad\ufffdbyte",
		"lots \t\n of   space":  "lots of space",
	} {
		if got := safe(in); got != want {
			t.Errorf("safe(%q) = %q, want %q", in, got, want)
		}
	}
	if got := safe(strings.Repeat("y", 300)); len([]rune(got)) != maxField || !strings.HasSuffix(got, "…") {
		t.Errorf("not capped: %d", len([]rune(got)))
	}
	// The message and With attributes are cleaned too; empty attrs and a
	// zero time are handled.
	var buf bytes.Buffer
	l := slog.New(New(&buf, Options{})).With("peer", "evil\u001b[2J")
	l.Info("hi\u202e\nthere", slog.Attr{})
	_ = New(&buf, Options{}).Handle(context.Background(), slog.NewRecord(time.Time{}, slog.LevelInfo, "zero", 0))
	if out := buf.String(); strings.ContainsAny(out, "\x1b\u202e") || !strings.Contains(out, "hi there  peer=evil[2J\n") ||
		strings.Contains(out, "=<nil>") || !strings.Contains(out, "--:--:-- zero") {
		t.Errorf("%q", out)
	}
}

func TestColor(t *testing.T) {
	var buf bytes.Buffer
	light := theme.Light
	story(slog.New(fixed{New(&buf, Options{Color: true, Palette: &light})}))
	out := buf.String()
	if !strings.Contains(out, "\x1b[") || strings.Contains(out, "\x1b[2J") {
		t.Errorf("colour missing, or the partner's escape kept:\n%q", out)
	}
	// The aurora's first stop on model lines; the light palette's green on VERIFIED.
	if !strings.Contains(out, "38;2;33;123;254") || !strings.Contains(out, "38;2;24;128;56") {
		t.Errorf("expected colours missing:\n%q", out)
	}
	// WithAttrs and WithGroup.
	buf.Reset()
	l := slog.New(New(&buf, Options{})).With("node", "cy").WithGroup("g")
	l.Info("hello", "k", 1)
	if !strings.Contains(buf.String(), "hello  node=cy  k=1") {
		t.Errorf("attrs: %q", buf.String())
	}
}
