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

package operator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
)

func statusReq(sender, ref string, q intents.TnTRequest) *node.Request {
	h := envelope.NewHeader(intents.OceanTnTRequest, sender, "ocean-blue.example", time.Now())
	h.BookingRef = &ref
	return &node.Request{Envelope: &envelope.Envelope{Header: h}, Body: &intents.TnTStatusRequestBody{TnTRequest: q}}
}

// TestStatusAuthorize: only the booking's shipper; the same refusal for
// every reason; an attached delegation nobody verified grants nothing,
// even to the shipper.
func TestStatusAuthorize(t *testing.T) {
	f := newInquiryFixture(t, nil)
	s := &StatusService{Bookings: f.svc.Bookings, Log: f.svc.Log}
	q := intents.TnTRequest{TransportDocumentReference: "OB-TD-1"}
	if err := s.Authorize(context.Background(), statusReq("cymbal-shoes.example", "OB-BKG-1", q)); err != nil {
		t.Errorf("the shipper: %v", err)
	}
	for name, req := range map[string]*node.Request{
		"a stranger":          statusReq("mallory.example", "OB-BKG-1", q),
		"another shipper's":   statusReq("cymbal-shoes.example", "OB-BKG-2", q),
		"an unknown booking":  statusReq("cymbal-shoes.example", "OB-BKG-404", q),
		"the operator itself": statusReq("ocean-blue.example", "OB-BKG-1", q),
	} {
		var rej *node.Rejection
		if err := s.Authorize(context.Background(), req); !errors.As(err, &rej) || rej.Code != intents.CodeForbidden || rej.Detail != notAParty.Detail {
			t.Errorf("%s: %v", name, err)
		}
	}
	d := statusReq("cymbal-shoes.example", "OB-BKG-1", q)
	d.Envelope.Attachments.Delegation = json.RawMessage(`{}`)
	if err := s.Authorize(context.Background(), d); err == nil {
		t.Error("a delegation was accepted")
	}
}

func TestStatusHandle(t *testing.T) {
	f := newInquiryFixture(t, nil)
	s := &StatusService{Bookings: f.svc.Bookings, Log: f.svc.Log}
	handle := func(q intents.TnTRequest) intents.TnTStatus {
		t.Helper()
		r, err := s.Handle(context.Background(), statusReq("cymbal-shoes.example", "OB-BKG-1", q))
		if err != nil || r.BookingRef != "OB-BKG-1" {
			t.Fatalf("Handle: %+v, %v", r, err)
		}
		body := r.Body.(*intents.TnTStatusResponseBody)
		if err := body.Validate(); err != nil {
			t.Errorf("the response is invalid: %v", err)
		}
		return body.TnTResponse
	}
	st := handle(intents.TnTRequest{TransportDocumentReference: "OB-TD-1", CarrierBookingReference: "OB-BKG-1"})
	// The fixture's log: gate-in, departure, an arrival estimate, an alert.
	if st.DCSAStatus != intents.StatusInTransit || st.VesselName != "Ocean Blue Aurora" || st.VoyageNumber != "42W" ||
		st.PortOfLoading != "NLRTM" || st.PortOfDischarge != "USEWR" || st.EstimatedTimeOfArrival != "2026-10-16T08:00:00Z" ||
		st.CurrentLocation == nil || st.CurrentLocation.UNLocationCode != "NLRTM" || len(st.Events) != 3 {
		t.Errorf("status %+v", st)
	}
	for _, e := range st.Events {
		if strings.Contains(e.FacilityName, "secret") {
			t.Error("another booking's event in the report")
		}
	}
	// Equipment: only that container's equipment events (transport events stay).
	if st := handle(intents.TnTRequest{TransportDocumentReference: "OB-TD-1", EquipmentReference: "OCBU0000004"}); len(st.Events) != 3 {
		t.Errorf("by container: %d events", len(st.Events))
	}
	for name, q := range map[string]intents.TnTRequest{
		"another document":  {TransportDocumentReference: "OB-TD-2"},
		"another booking":   {TransportDocumentReference: "OB-TD-1", CarrierBookingReference: "OB-BKG-2"},
		"another container": {TransportDocumentReference: "OB-TD-1", EquipmentReference: "MSKU0000001"},
	} {
		if st := handle(q); st.DCSAStatus != intents.StatusNotFound || len(st.Events) != 0 || st.VesselName != "" {
			t.Errorf("%s: %+v", name, st)
		}
	}
}

// TestStatusProgress: the status follows the latest actual event, and
// the report keeps the latest events within the cap.
func TestStatusProgress(t *testing.T) {
	f := newInquiryFixture(t, nil)
	s := &StatusService{Bookings: f.svc.Bookings, Log: f.svc.Log}
	b, _ := f.svc.Bookings.Get("ocean-blue.example", "OB-BKG-2")
	if st := s.status(t, b.Ref, "other-shipper.example"); st.DCSAStatus != intents.StatusReceived {
		t.Errorf("after gate-in only: %s", st.DCSAStatus)
	}
	for i := range intents.MaxStatusEvents + 5 {
		e := ev(fmt.Sprintf("EV-9%04d", i))
		e.EventTypeCode = []string{"LOAD", "DISC", "GTOT"}[i%3]
		raw, _ := json.Marshal(e)
		_, _ = f.svc.Log.Add("ocean-blue.example", b.Ref, store.Record{ID: e.EventID, Intent: intents.TnTEvent, Body: raw})
	}
	st := s.status(t, b.Ref, "other-shipper.example")
	if len(st.Events) != intents.MaxStatusEvents || st.Events[len(st.Events)-1].EventID != fmt.Sprintf("EV-9%04d", intents.MaxStatusEvents+4) {
		t.Errorf("%d events, last %s", len(st.Events), st.Events[len(st.Events)-1].EventID)
	}
	if st.DCSAStatus != intents.StatusDelivered { // the last one, EV-90104, is a gate-out
		t.Errorf("status %s", st.DCSAStatus)
	}
}

func (s *StatusService) status(t *testing.T, ref, shipper string) intents.TnTStatus {
	t.Helper()
	r, err := s.Handle(context.Background(), statusReq(shipper, ref, intents.TnTRequest{TransportDocumentReference: "OB-TD" + strings.TrimPrefix(ref, "OB-BKG")}))
	if err != nil {
		t.Fatal(err)
	}
	return r.Body.(*intents.TnTStatusResponseBody).TnTResponse
}

// TestStatusDetails: another container's equipment events are filtered
// out; an estimate moves neither the status nor the location; a shipment
// event moves the status but not the location.
func TestStatusDetails(t *testing.T) {
	f := newInquiryFixture(t, nil)
	s := &StatusService{Bookings: f.svc.Bookings, Log: f.svc.Log}
	b, _ := f.svc.Bookings.Get("ocean-blue.example", "OB-BKG-2")
	b2 := b
	b2.Ref, b2.Shipper = "OB-BKG-3", "third.example"
	b2.Confirmation.BookingRef = "OB-BKG-3"
	b2.Confirmation.EBL.TransportDocumentReference = "OB-TD-3"
	b2.Confirmation.EBL.Containers = []intents.ShippingContainer{{ContainerNumber: "OCBU0000004", EquipmentSizeType: "45G1", SealNumber: "S1", GrossWeightKg: 12000}, {ContainerNumber: "OCBU0000015", EquipmentSizeType: "45G1", SealNumber: "S2", GrossWeightKg: 12000}}
	if err := f.svc.Bookings.Add(b2); err != nil {
		t.Fatal(err)
	}
	add := func(e *intents.TnTEventBody) {
		raw, _ := json.Marshal(e)
		_, _ = f.svc.Log.Add("ocean-blue.example", "OB-BKG-3", store.Record{ID: e.EventID, Intent: intents.TnTEvent, Body: raw})
	}
	gtin := ev("EV-1")
	add(gtin)
	other := ev("EV-2")
	other.EquipmentReference = "OCBU0000015"
	add(other)
	est := ev("EV-3")
	est.EventTypeCode, est.EventClassifierCode, est.UNLocationCode = "DISC", intents.ClassifierEstimated, "USEWR"
	add(est)
	conf := &intents.TnTEventBody{EventID: "EV-4", EventType: intents.EventTypeShipment, EventClassifierCode: intents.ClassifierActual,
		EventTypeCode: "HOLD", EventDateTime: "2026-10-07T12:00:00Z", Description: "Customs hold", UNLocationCode: "BEANR"}
	add(conf)
	r, err := s.Handle(context.Background(), statusReq("third.example", "OB-BKG-3", intents.TnTRequest{TransportDocumentReference: "OB-TD-3", EquipmentReference: "OCBU0000004"}))
	if err != nil {
		t.Fatal(err)
	}
	st := r.Body.(*intents.TnTStatusResponseBody).TnTResponse
	var ids []string
	for _, e := range st.Events {
		ids = append(ids, e.EventID)
	}
	if strings.Join(ids, ",") != "EV-1,EV-3,EV-4" {
		t.Errorf("events %v: another container's should be filtered", ids)
	}
	if st.DCSAStatus != intents.StatusReceived || st.CurrentLocation == nil || st.CurrentLocation.UNLocationCode != "NLRTM" {
		t.Errorf("status %s at %+v: an estimate or a shipment event moved it", st.DCSAStatus, st.CurrentLocation)
	}
}

// TestStatusFitsAnEnvelope: a report of 100 events at their size limits
// is cut until it seals within the envelope limit.
func TestStatusFitsAnEnvelope(t *testing.T) {
	f := newInquiryFixture(t, nil)
	s := &StatusService{Bookings: f.svc.Bookings, Log: f.svc.Log}
	for i := range intents.MaxStatusEvents {
		e := ev(fmt.Sprintf("EV-B%04d", i))
		// 2-byte runes, and characters JSON escapes to six bytes.
		e.Description, e.FacilityName, e.VesselName = strings.Repeat("é<", 140), strings.Repeat("ü", 128), strings.Repeat("&", 128)
		raw, _ := json.Marshal(e)
		_, _ = f.svc.Log.Add("ocean-blue.example", "OB-BKG-2", store.Record{ID: e.EventID, Intent: intents.TnTEvent, Body: raw})
	}
	r, err := s.Handle(context.Background(), statusReq("other-shipper.example", "OB-BKG-2", intents.TnTRequest{TransportDocumentReference: "OB-TD-2"}))
	if err != nil {
		t.Fatal(err)
	}
	body := r.Body.(*intents.TnTStatusResponseBody)
	raw, _ := json.Marshal(body)
	if len(raw) > maxStatusBody || len(body.TnTResponse.Events) == 0 || len(body.TnTResponse.Events) == intents.MaxStatusEvents+1 {
		t.Errorf("%d bytes, %d events", len(raw), len(body.TnTResponse.Events))
	}
	if err := body.Validate(); err != nil {
		t.Error(err)
	}
	last := body.TnTResponse.Events[len(body.TnTResponse.Events)-1].EventID
	if last != fmt.Sprintf("EV-B%04d", intents.MaxStatusEvents-1) {
		t.Errorf("the latest event was dropped: %s", last)
	}
	if envelope.MaxSize*3/4 < maxStatusBody+4<<10 {
		t.Error("the body budget leaves no room for the header within the envelope limit")
	}
}
