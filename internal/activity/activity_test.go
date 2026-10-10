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

package activity

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/supercargo/pkg/intents"
)

func envelope(id string) Entry {
	return Entry{Kind: KindEnvelope, Envelope: &Envelope{Direction: "in", Intent: intents.TnTEvent, MessageID: id, Outcome: "accepted"}}
}

func TestBusHistoryAndSubscribe(t *testing.T) {
	b := NewBus(3)
	var sunk []uint64
	b.Sink(func(e Entry) { sunk = append(sunk, e.Seq) })
	for i := range 5 {
		b.Publish(envelope(string(rune('a' + i))))
	}
	if b.Seq() != 5 || len(sunk) != 5 || sunk[4] != 5 {
		t.Fatalf("seq %d, sunk %v", b.Seq(), sunk)
	}
	// Only the last 3 are kept. Since 0 sees 3..5 and no gap (a new
	// watcher missed nothing it asked for); since 1 sees a gap.
	past, live, gap, cancel := b.Subscribe(0, 4)
	if len(past) != 3 || past[0].Seq != 3 || gap {
		t.Errorf("since 0: %d past (first %d), gap %v", len(past), past[0].Seq, gap)
	}
	if _, _, gap1, c := b.Subscribe(1, 0); !gap1 {
		t.Error("since 1 with 3.. kept: no gap")
	} else {
		c()
	}
	// A since beyond the last entry (the node restarted): everything, and
	// a gap.
	if all, _, gapR, c := b.Subscribe(99, 0); !gapR || len(all) != 3 {
		t.Errorf("since 99: %d entries, gap %v", len(all), gapR)
	} else {
		c()
	}
	if past[0].Time.IsZero() {
		t.Error("entries aren't stamped with a time")
	}
	b.Publish(envelope("f"))
	if e := <-live; e.Seq != 6 {
		t.Errorf("live got %d", e.Seq)
	}
	cancel()
	cancel() // idempotent
	if _, ok := <-live; ok {
		t.Error("the channel is open after cancel")
	}
	// Since the latest kept: nothing missed.
	_, _, gap, cancel = b.Subscribe(4, 0)
	cancel()
	if gap {
		t.Error("since 4 reported a gap with 4.. kept")
	}
	// A fresh bus has no gap.
	if _, _, gap, c := NewBus(0).Subscribe(0, 0); gap {
		t.Error("a fresh bus reported a gap")
	} else {
		c()
	}
}

// TestSlowSubscriberIsCutOff: publishing never blocks; a subscriber that
// falls behind has its channel closed, and catches up from the history.
func TestSlowSubscriberIsCutOff(t *testing.T) {
	b := NewBus(0)
	_, live, _, cancel := b.Subscribe(0, 2)
	defer cancel()
	done := make(chan struct{})
	go func() {
		for i := range 10 {
			b.Publish(envelope(string(rune('a' + i))))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a slow subscriber")
	}
	n := 0
	for range live {
		n++
	}
	if n != 2 {
		t.Errorf("the slow subscriber got %d entries before being cut off, want its buffer of 2", n)
	}
	past, _, _, c := b.Subscribe(2, 0)
	c()
	if len(past) != 8 || past[0].Seq != 3 {
		t.Errorf("catching up from 2: %d entries", len(past))
	}
}

// TestSlogSink: the log lines are the node log's, message and attributes.
func TestSlogSink(t *testing.T) {
	var buf bytes.Buffer
	sink := SlogSink(slog.New(slog.NewJSONHandler(&buf, nil)))
	ev := &intents.TnTEventBody{EventID: "EV-1", EventType: "EQUIPMENT", EventTypeCode: "GTIN", EventClassifierCode: "ACT",
		EventDateTime: "2026-10-07T11:00:00Z", UNLocationCode: "NLRTM", Description: "An event", EquipmentReference: "OCBU0000004"}
	dep := &intents.TnTEventBody{EventID: "EV-2", EventType: "TRANSPORT", EventTypeCode: "DEPA", EventClassifierCode: "ACT",
		EventDateTime: "2026-10-07T18:00:00Z", UNLocationCode: "NLRTM", Description: "An event", VesselName: "Aurora", VoyageNumber: "42W"}
	al := &intents.TnTAlertBody{AlertID: "AL-1", Kind: "delay", Severity: "high", Summary: "Late.", PreviousETA: "a", NewETA: "b", CauseEventIDs: []string{"EV-2"}}
	for _, e := range []Entry{
		{Kind: KindEnvelope, Envelope: &Envelope{Direction: "in", Intent: "ulp.tnt.inquiry", Sender: "x.example", Via: "a2a", Verified: true, Outcome: "rejected", Code: "forbidden", Reason: "not a party"}},
		{Kind: KindRecord, Record: &Record{Action: ActionRecorded, Booking: "OB-1", Event: ev}},
		{Kind: KindRecord, Record: &Record{Action: ActionPublished, Booking: "OB-1", Event: dep}},
		{Kind: KindRecord, Record: &Record{Action: ActionRecorded, Booking: "OB-1", Alert: al}},
		{Kind: KindModel, Model: &Model{Agent: "assistant · alex", Tools: []string{"draft_booking"}, Booking: "OB-1", Attempt: 2, Model: "fake", PromptHash: "abc", Skills: []string{"s@1"}, LatencyMS: 1500, Outcome: "invalid", Reason: "bad"}},
		{Kind: KindVoyage, Voyage: &Voyage{Booking: "OB-1", Started: true, Departure: "d", Arrival: "a", HoursPerSecond: 2}},
		{Kind: KindVoyage, Voyage: &Voyage{Booking: "OB-2", Error: "too many"}},
		{Kind: KindPush, Push: &Push{Booking: "OB-1", ID: "EV-3", Problem: "dropped", Error: "refused"}},
		{Kind: KindPush, Push: &Push{Booking: "OB-1", Problem: "unpublished", Error: "full"}},
		{Kind: KindSummary, Summary: &Summary{Booking: "OB-1", AlertID: "AL-1", Text: "Late.", Impact: "misses", GeneratedBy: "llm", Cited: []string{"AL-1"}}},
		{Kind: KindTool, Tool: &Tool{Via: "mcp", Name: "draft_booking", Outcome: "drafted", Draft: "MD-0001"}},
		{Kind: KindTool, Tool: &Tool{Via: "mcp", Name: "ask_operator", Outcome: "refused", Reason: "too many"}},
	} {
		sink(e)
	}
	var lines []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		_ = json.Unmarshal([]byte(l), &m)
		lines = append(lines, m)
	}
	want := []struct{ msg, level, key, value string }{
		{"envelope", "WARN", "via", "a2a"},
		{"event recorded", "INFO", "container", "OCBU0000004"},
		{"event published", "INFO", "vessel", "Aurora 42W"},
		{"alert recorded", "WARN", "severity", "high"},
		{"model call", "WARN", "latency", "1.5s"},
		{"voyage started", "INFO", "booking", "OB-1"},
		{"no voyage for booking", "WARN", "err", "too many"},
		{"push dropped", "WARN", "id", "EV-3"},
		{"event not published", "WARN", "err", "full"},
		{"alert summary", "WARN", "impact", "misses"},
		{"tool call", "INFO", "draft", "MD-0001"},
		{"tool call", "WARN", "reason", "too many"},
	}
	if len(lines) != len(want) {
		t.Fatalf("%d lines, want %d:\n%s", len(lines), len(want), buf.String())
	}
	for i, w := range want {
		if lines[i]["msg"] != w.msg || lines[i]["level"] != w.level || lines[i][w.key] != w.value {
			t.Errorf("line %d: %v, want msg %q level %s %s=%q", i, lines[i], w.msg, w.level, w.key, w.value)
		}
	}
	if lines[4]["agent"] != "assistant · alex" || fmt.Sprint(lines[4]["tools"]) != "[draft_booking]" {
		t.Errorf("model line %v", lines[4])
	}
	if lines[0]["code"] != "forbidden" || lines[0]["reason"] != "not a party" || lines[0]["verified"] != true {
		t.Errorf("envelope line %v", lines[0])
	}
	// An entry of no known shape logs nothing.
	buf.Reset()
	sink(Entry{Kind: "unknown"})
	if buf.Len() != 0 {
		t.Errorf("an empty entry logged %q", buf.String())
	}
}
