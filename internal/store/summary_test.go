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

package store

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
)

func eventRecord(t *testing.T, id, code, class, at string) Record {
	t.Helper()
	typ := "EQUIPMENT"
	if code == "DEPA" || code == "ARRI" {
		typ = "TRANSPORT"
	}
	b, _ := json.Marshal(intents.TnTEventBody{EventID: id, EventType: typ, EventCode: code, EventClassifierCode: class, EventDateTime: at})
	return Record{ID: id, Intent: intents.TnTEvent, Body: b, At: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC),
		Header: envelope.Header{Sender: "ocean-blue.example", MessageID: "m-" + id, SentAt: "2026-10-07T12:00:00.000Z"}}
}

func TestSummarize(t *testing.T) {
	b := Booking{Ref: "OB-1", Operator: "ocean-blue.example", Shipper: "cymbal-shoes.example",
		Request: intents.BookingRequestBody{Origin: intents.Location{UNLocode: "NLRTM"}, Destination: intents.Location{UNLocode: "USEWR"}},
		Confirmation: intents.BookingConfirmationBody{Status: "confirmed", Vessel: &intents.Vessel{Name: "Aurora", Voyage: "42W"},
			PlannedDeparture: "2026-10-07T18:00:00Z", PlannedArrival: "2026-10-13T08:00:00Z", EquipmentRefs: []string{"OCBU0000004"}}}
	s := Summarize(b, nil)
	if s.Status != "confirmed" || s.ETA != "2026-10-13T08:00:00Z" || s.ETASource != "" || s.Vessel != "Aurora" || len(s.Containers) != 1 {
		t.Errorf("no events: %+v", s)
	}
	recs := []Record{
		eventRecord(t, "EV-1", "GTIN", "ACT", "2026-10-06T16:00:00Z"),
		eventRecord(t, "EV-2", "DEPA", "ACT", "2026-10-07T18:00:00Z"),
		eventRecord(t, "EV-3", "ARRI", "EST", "2026-10-13T08:00:00Z"),
		{ID: "AL-4", Intent: intents.TnTAlert, Body: json.RawMessage(`{}`)},
		eventRecord(t, "EV-5", "ARRI", "EST", "2026-10-16T08:00:00Z"),
		{ID: "EV-x", Intent: intents.TnTEvent, Body: json.RawMessage(`not json`)},
	}
	s = Summarize(b, recs)
	if s.Status != "in transit" || s.ETA != "2026-10-16T08:00:00Z" || s.ETASource != "EV-5" || s.Events != 4 || s.Alerts != 1 ||
		s.LastEvent != "TRANSPORT ARRI EST" {
		t.Errorf("in transit: %+v", s)
	}
	// After arrival, a later estimate doesn't move the ETA.
	recs = append(recs, eventRecord(t, "EV-6", "ARRI", "ACT", "2026-10-16T09:00:00Z"), eventRecord(t, "EV-7", "ARRI", "EST", "2026-10-20T00:00:00Z"),
		eventRecord(t, "EV-8", "GTOT", "ACT", "2026-10-17T15:00:00Z"))
	s = Summarize(b, recs)
	if s.Status != "gated out" || s.ETA != "2026-10-16T09:00:00Z" || s.ETASource != "EV-6" {
		t.Errorf("delivered: %+v", s)
	}
	if s := Summarize(Booking{Ref: "x"}, nil); s.Containers == nil || s.Vessel != "" {
		t.Errorf("empty booking: %+v", s)
	}
}

func TestViewRecordsAndList(t *testing.T) {
	v := ViewRecords([]Record{eventRecord(t, "EV-1", "GTIN", "ACT", "2026-10-06T16:00:00Z")})
	if len(v) != 1 || v[0].Sender != "ocean-blue.example" || v[0].MessageID != "m-EV-1" || v[0].RecordedAt != "2026-10-07T12:00:00Z" ||
		v[0].Intent != intents.TnTEvent {
		t.Errorf("views %+v", v)
	}
	if v := ViewRecords(nil); v == nil {
		t.Error("ViewRecords(nil) is nil, want empty")
	}
	s := NewBookings()
	t0 := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	_ = s.Add(Booking{Ref: "A", Operator: "o.example", CreatedAt: t0})
	_ = s.Add(Booking{Ref: "B", Operator: "o.example", CreatedAt: t0.Add(time.Hour)})
	_ = s.Add(Booking{Ref: "C", Operator: "o.example", CreatedAt: t0})
	got := s.List()
	if len(got) != 3 || got[0].Ref != "B" || got[1].Ref != "A" || got[2].Ref != "C" {
		t.Errorf("List order %v %v %v", got[0].Ref, got[1].Ref, got[2].Ref)
	}
}
