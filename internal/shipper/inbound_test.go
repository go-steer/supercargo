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

package shipper

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
)

func inbound(t *testing.T, subIntents ...string) (*Inbound, *int) {
	t.Helper()
	bookings := store.NewBookings()
	b := store.Booking{Ref: "OB-BKG-1", Operator: "ocean-blue.example", Shipper: "cymbal-shoes.example"}
	if subIntents != nil {
		b.Request.Subscription = &intents.Subscription{Endpoint: "https://cymbal-shoes.example/ulp", Intents: subIntents}
	}
	if err := bookings.Add(b); err != nil {
		t.Fatal(err)
	}
	alerts := 0
	return &Inbound{Bookings: bookings, Log: store.NewEventLog(0), Now: time.Now,
		OnAlert: func(store.Booking, *intents.TnTAlertBody) { alerts++ }}, &alerts
}

func pushReq(sender, ref string, body intents.Body) *node.Request {
	h := envelope.NewHeader(body.Intent(), sender, "cymbal-shoes.example", time.Now())
	h.BookingRef = &ref
	raw, _ := json.Marshal(body)
	return &node.Request{Envelope: &envelope.Envelope{Header: h, Body: raw}, Body: body}
}

var (
	anEvent = &intents.TnTEventBody{EventID: "EV-1", EventType: "EQUIPMENT", EventClassifierCode: "ACT", EventTypeCode: "GTIN",
		EventDateTime: "2026-10-07T11:20:00Z", UNLocationCode: "NLRTM", FacilityName: "R", Description: "An event", EquipmentReference: "OCBU0000004"}
	anAlert = &intents.TnTAlertBody{AlertID: "AL-1", Kind: "exception", Severity: "low", Summary: "Hold", CauseEventIDs: []string{}}
)

func TestInboundAuthorize(t *testing.T) {
	in, _ := inbound(t, intents.TnTEvent)
	if err := in.Authorize(context.Background(), pushReq("ocean-blue.example", "OB-BKG-1", anEvent)); err != nil {
		t.Errorf("the operator, subscribed: %v", err)
	}
	for name, req := range map[string]*node.Request{
		"another sender":     pushReq("other-carrier.example", "OB-BKG-1", anEvent),
		"unknown booking":    pushReq("ocean-blue.example", "OB-BKG-9", anEvent),
		"unsubscribed alert": pushReq("ocean-blue.example", "OB-BKG-1", anAlert),
	} {
		var r *node.Rejection
		if err := in.Authorize(context.Background(), req); !errors.As(err, &r) || r.Code != intents.CodeForbidden {
			t.Errorf("%s: %v, want forbidden", name, err)
		}
	}
	noSub, _ := inbound(t)
	if err := noSub.Authorize(context.Background(), pushReq("ocean-blue.example", "OB-BKG-1", anEvent)); err == nil {
		t.Error("a booking without a subscription accepted a push")
	}
}

func TestInboundHandle(t *testing.T) {
	in, alerts := inbound(t, intents.TnTEvent, intents.TnTAlert)
	for range 2 { // the second time is a duplicate: recorded once, hook run once
		for _, body := range []intents.Body{anEvent, anAlert} {
			reply, err := in.Handle(context.Background(), pushReq("ocean-blue.example", "OB-BKG-1", body))
			if err != nil || reply != nil {
				t.Fatalf("Handle = %v, %v", reply, err)
			}
		}
	}
	recs := in.Log.List("ocean-blue.example", "OB-BKG-1")
	if len(recs) != 2 || recs[0].ID != "EV-1" || recs[1].ID != "AL-1" || recs[0].Header.Sender != "ocean-blue.example" {
		t.Errorf("records %+v", recs)
	}
	if *alerts != 1 {
		t.Errorf("OnAlert ran %d times, want 1", *alerts)
	}
	// OnEvent likewise runs once per new event, with the booking.
	var seen []string
	in.OnEvent = func(b store.Booking, e *intents.TnTEventBody) { seen = append(seen, b.Ref+" "+e.EventID) }
	ev2 := *anEvent
	ev2.EventID = "EV-2"
	for range 2 {
		if _, err := in.Handle(context.Background(), pushReq("ocean-blue.example", "OB-BKG-1", &ev2)); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 1 || seen[0] != "OB-BKG-1 EV-2" {
		t.Errorf("OnEvent saw %v", seen)
	}
	// With no hooks set, new pushes are still recorded.
	in.OnAlert, in.OnEvent = nil, nil
	al2 := *anAlert
	al2.AlertID = "AL-2"
	if _, err := in.Handle(context.Background(), pushReq("ocean-blue.example", "OB-BKG-1", &al2)); err != nil {
		t.Fatal(err)
	}
	if n := len(in.Log.List("ocean-blue.example", "OB-BKG-1")); n != 4 {
		t.Errorf("%d records, want 4", n)
	}
	if _, err := in.Handle(context.Background(), &node.Request{
		Envelope: pushReq("ocean-blue.example", "OB-BKG-1", anEvent).Envelope,
		Body:     &intents.ErrorBody{Code: "internal", Detail: "x"},
	}); err == nil {
		t.Error("handled a body that isn't a push")
	}
	full, _ := inbound(t, intents.TnTEvent)
	full.Log = store.NewEventLog(1)
	_, _ = full.Handle(context.Background(), pushReq("ocean-blue.example", "OB-BKG-1", anEvent))
	second := *anEvent
	second.EventID = "EV-2"
	if _, err := full.Handle(context.Background(), pushReq("ocean-blue.example", "OB-BKG-1", &second)); err == nil {
		t.Error("recorded past a full log")
	}
}
