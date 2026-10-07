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
	"strings"
	"time"

	"github.com/go-steer/supercargo/internal/ports"
	"github.com/go-steer/supercargo/pkg/intents"
)

// Summary is a booking at a glance, from its record and its events: what
// a shipments list or a sidebar shows.
type Summary struct {
	Operator string `json:"operator"`
	Shipper  string `json:"shipper"`
	Ref      string `json:"bookingRef"`
	// TransportDocumentReference is the eBL's, once confirmed.
	TransportDocumentReference string           `json:"transportDocumentReference,omitempty"`
	Status                     string           `json:"status"`
	Origin                     intents.Location `json:"origin"`
	Destination                intents.Location `json:"destination"`
	Vessel                     string           `json:"vessel,omitempty"`
	Voyage                     string           `json:"voyage,omitempty"`
	PlannedDeparture           string           `json:"plannedDeparture,omitempty"`
	PlannedArrival             string           `json:"plannedArrival,omitempty"`
	// ETA is the latest arrival on record: the actual arrival, else the
	// latest estimate, else the plan. ETASource is the event it came from.
	ETA        string   `json:"eta,omitempty"`
	ETASource  string   `json:"etaSource,omitempty"`
	LastEvent  string   `json:"lastEvent,omitempty"` // e.g. "TRANSPORT DEPA ACT"
	LastAt     string   `json:"lastAt,omitempty"`
	Events     int      `json:"events"`
	Alerts     int      `json:"alerts"`
	Containers []string `json:"containers"`
	CreatedAt  string   `json:"createdAt"`
}

// RecordView is a record as the control API shows it: the stored body as
// it was verified (received) or published, with the envelope facts that
// say who sent it.
type RecordView struct {
	ID         string          `json:"id"`
	Intent     string          `json:"intent"`
	Sender     string          `json:"sender,omitempty"`
	MessageID  string          `json:"messageId,omitempty"`
	SentAt     string          `json:"sentAt,omitempty"`
	RecordedAt string          `json:"recordedAt"`
	Body       json.RawMessage `json:"body"`
}

// ViewRecords returns recs as views.
func ViewRecords(recs []Record) []RecordView {
	out := make([]RecordView, 0, len(recs))
	for _, r := range recs {
		out = append(out, RecordView{ID: r.ID, Intent: r.Intent, Sender: r.Header.Sender, MessageID: r.Header.MessageID,
			SentAt: r.Header.SentAt, RecordedAt: r.At.UTC().Format(time.RFC3339), Body: r.Body})
	}
	return out
}

// Statuses a summary reports, from the latest actual event.
var statuses = map[string]string{
	"GTIN": "at origin terminal",
	"LOAD": "loaded",
	"DEPA": "in transit",
	"ARRI": "arrived",
	"DISC": "discharged",
	"GTOT": "gated out",
}

// Summarize sums up b from its records.
func Summarize(b Booking, recs []Record) Summary {
	c := b.Confirmation
	s := Summary{Operator: b.Operator, Shipper: b.Shipper, Ref: b.Ref, Status: c.Status,
		TransportDocumentReference: c.TransportDocumentReference(),
		Origin:                     ports.Location(b.Request.PortOfLoading), Destination: ports.Location(b.Request.PortOfDischarge),
		PlannedDeparture: c.PlannedDeparture, PlannedArrival: c.PlannedArrival, ETA: c.PlannedArrival,
		Containers: c.EquipmentRefs(), CreatedAt: b.CreatedAt.UTC().Format(time.RFC3339)}
	s.Vessel, s.Voyage = c.Vessel()
	if s.Containers == nil {
		s.Containers = []string{}
	}
	arrived := false
	for _, r := range recs {
		switch r.Intent {
		case intents.TnTAlert:
			s.Alerts++
		case intents.TnTEvent:
			var e intents.TnTEventBody
			if json.Unmarshal(r.Body, &e) != nil {
				continue // the node's own, validated when stored
			}
			s.Events++
			s.LastEvent = strings.Join([]string{e.EventType, e.EventTypeCode, e.EventClassifierCode}, " ")
			s.LastAt = e.EventDateTime
			if e.EventClassifierCode == intents.ClassifierActual {
				if st, ok := statuses[e.EventTypeCode]; ok {
					s.Status = st
				}
			}
			if e.EventTypeCode == "ARRI" && !arrived {
				s.ETA, s.ETASource = e.EventDateTime, e.EventID
				arrived = e.EventClassifierCode == intents.ClassifierActual
			}
		}
	}
	return s
}
