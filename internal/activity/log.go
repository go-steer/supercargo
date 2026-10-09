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
	"context"
	"log/slog"
	"strings"
	"time"
)

// SlogSink writes each entry to l as one structured record. The messages
// and attributes are the node log's (scripts and the smoke tests read
// them), whichever renderer the logger uses.
func SlogSink(l *slog.Logger) func(Entry) {
	ctx := context.Background()
	return func(e Entry) {
		switch {
		case e.Envelope != nil:
			v := e.Envelope
			level := slog.LevelInfo
			if v.Outcome == "rejected" || v.Outcome == "malformed" || v.Outcome == "failed" {
				level = slog.LevelWarn
			}
			l.LogAttrs(ctx, level, "envelope",
				slog.String("dir", v.Direction), slog.String("intent", v.Intent),
				slog.String("sender", v.Sender), slog.String("receiver", v.Receiver),
				slog.String("via", v.Via), slog.String("messageId", v.MessageID),
				slog.String("kid", v.KID), slog.Bool("verified", v.Verified),
				slog.String("outcome", v.Outcome), slog.String("code", v.Code),
				slog.String("reason", v.Reason))
		case e.Record != nil && e.Record.Event != nil:
			r, ev := e.Record, e.Record.Event
			attrs := []slog.Attr{slog.String("booking", r.Booking), slog.String("eventId", ev.EventID),
				slog.String("event", ev.EventType+" "+ev.EventTypeCode+" "+ev.EventClassifierCode),
				slog.String("at", ev.EventDateTime), slog.String("location", ev.UNLocationCode)}
			if ev.EquipmentReference != "" {
				attrs = append(attrs, slog.String("container", ev.EquipmentReference))
			}
			if ev.VesselName != "" {
				attrs = append(attrs, slog.String("vessel", strings.TrimSpace(ev.VesselName+" "+ev.VoyageNumber)))
			}
			l.LogAttrs(ctx, slog.LevelInfo, "event "+r.Action, attrs...)
		case e.Record != nil && e.Record.Alert != nil:
			r, al := e.Record, e.Record.Alert
			// An alert is news: Warn.
			l.LogAttrs(ctx, slog.LevelWarn, "alert "+r.Action,
				slog.String("booking", r.Booking), slog.String("alertId", al.AlertID),
				slog.String("kind", al.Kind), slog.String("severity", al.Severity),
				slog.String("summary", al.Summary), slog.String("previousEta", al.PreviousETA),
				slog.String("newEta", al.NewETA), slog.Any("causeEventIds", al.CauseEventIDs))
		case e.Model != nil:
			m := e.Model
			level := slog.LevelInfo
			if m.Outcome != "ok" {
				level = slog.LevelWarn
			}
			attrs := []slog.Attr{slog.String("agent", m.Agent), slog.String("booking", m.Booking), slog.Int("attempt", m.Attempt), slog.String("model", m.Model),
				slog.String("promptHash", m.PromptHash), slog.Any("skills", m.Skills),
				slog.Int("inputTokens", m.InputTokens), slog.Int("outputTokens", m.OutputTokens),
				slog.String("latency", (time.Duration(m.LatencyMS) * time.Millisecond).String()),
				slog.String("outcome", m.Outcome), slog.String("reason", m.Reason)}
			if len(m.Tools) > 0 {
				attrs = append(attrs, slog.Any("tools", m.Tools))
			}
			l.LogAttrs(ctx, level, "model call", attrs...)
		case e.Tool != nil:
			t := e.Tool
			level := slog.LevelInfo
			if t.Outcome == "refused" {
				level = slog.LevelWarn
			}
			l.LogAttrs(ctx, level, "tool call", slog.String("via", t.Via), slog.String("tool", t.Name),
				slog.String("outcome", t.Outcome), slog.String("reason", t.Reason), slog.String("draft", t.Draft))
		case e.Voyage != nil && e.Voyage.Started:
			v := e.Voyage
			l.LogAttrs(ctx, slog.LevelInfo, "voyage started", slog.String("booking", v.Booking),
				slog.String("departure", v.Departure), slog.String("arrival", v.Arrival),
				slog.Float64("hoursPerSecond", v.HoursPerSecond))
		case e.Voyage != nil:
			l.LogAttrs(ctx, slog.LevelWarn, "no voyage for booking", slog.String("booking", e.Voyage.Booking),
				slog.String("err", e.Voyage.Error))
		case e.Push != nil && e.Push.Problem == "dropped":
			l.LogAttrs(ctx, slog.LevelWarn, "push dropped", slog.String("booking", e.Push.Booking),
				slog.String("id", e.Push.ID), slog.String("err", e.Push.Error))
		case e.Summary != nil:
			v := e.Summary
			l.LogAttrs(ctx, slog.LevelWarn, "alert summary", slog.String("booking", v.Booking),
				slog.String("alertId", v.AlertID), slog.String("impact", v.Impact),
				slog.String("generatedBy", v.GeneratedBy), slog.String("text", v.Text),
				slog.Any("cited", v.Cited), slog.Any("unheldCauses", v.UnheldCauses))
		case e.Push != nil:
			l.LogAttrs(ctx, slog.LevelWarn, "event not published", slog.String("booking", e.Push.Booking),
				slog.String("err", e.Push.Error))
		}
	}
}
