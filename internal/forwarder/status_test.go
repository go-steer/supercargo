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

package forwarder

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
)

func statusFrom(sender, ref string) *node.Request {
	h := envelope.NewHeader(intents.ForwardingStatusRequest, sender, meridian, time.Now())
	return &node.Request{Envelope: &envelope.Envelope{Header: h}, Body: &intents.ForwardingStatusRequestBody{ShipperRef: ref}}
}

// TestStatus: the shipper finds its order by its own reference, with the
// message it went in and its current answer; anyone else, or another
// reference, finds nothing.
func TestStatus(t *testing.T) {
	o, _ := desk(t)
	order := orderFrom(cymbal, anOrder())
	if _, err := o.Handle(context.Background(), order); err != nil {
		t.Fatal(err)
	}
	s := &Status{Orders: o, Log: store.NewEventLog(0)}
	ask := func(sender, ref string) *node.Reply {
		t.Helper()
		req := statusFrom(sender, ref)
		if err := s.Authorize(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		r, err := s.Handle(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Body.Validate(); err != nil {
			t.Fatalf("an invalid response: %v", err)
		}
		return r
	}
	r := ask(cymbal, "CY-PO-55120")
	st := r.Body.(*intents.ForwardingStatusResponseBody)
	if !st.Found() || st.OrderRef != "MF-0007" || r.BookingRef != "MF-0007" || st.OrderMessageID != order.Envelope.Header.MessageID ||
		st.Answer.Status != intents.StatusPending || st.EstimatedDelivery != "" {
		t.Errorf("pending: %+v", st)
	}
	for name, who := range map[string][2]string{"another shipper": {"mallory.example", "CY-PO-55120"}, "another reference": {cymbal, "CY-PO-99999"}} {
		r := ask(who[0], who[1])
		if st := r.Body.(*intents.ForwardingStatusResponseBody); st.Found() || r.BookingRef != "" || st.ShipperRef != who[1] {
			t.Errorf("%s: %+v", name, st)
		}
	}
	// Confirmed, then delayed: the plan, and the delivery as the latest
	// alert moved it.
	_, _ = o.Bookings.Update(meridian, "MF-0007", func(b *store.Booking) error {
		c := *aPlan()
		c.OrderRef, c.SubscriptionID = "MF-0007", b.Order.Confirmation.SubscriptionID
		b.Order.Confirmation = c
		return nil
	})
	alert, _ := json.Marshal(intents.TnTAlertBody{AlertID: "MF-AL-1", Kind: intents.AlertDelay, Severity: intents.SeverityHigh, Summary: "Later.",
		PreviousETA: "2026-10-23T08:00:00Z", NewETA: "2026-10-26T08:00:00Z"})
	_, _ = s.Log.Add(meridian, "MF-0007", store.Record{ID: "MF-AL-1", Intent: intents.TnTAlert, Body: alert})
	st = ask(cymbal, "CY-PO-55120").Body.(*intents.ForwardingStatusResponseBody)
	if st.Answer.Status != intents.StatusConfirmed || len(st.Answer.Legs) != 1 || st.EstimatedDelivery != "2026-10-26T08:00:00Z" {
		t.Errorf("confirmed: %+v", st)
	}
	// Two orders under one reference: the latest.
	time.Sleep(10 * time.Millisecond)
	if _, err := o.Handle(context.Background(), orderFrom(cymbal, anOrder())); err != nil {
		t.Fatal(err)
	}
	if st := ask(cymbal, "CY-PO-55120").Body.(*intents.ForwardingStatusResponseBody); st.OrderRef == "MF-0007" {
		t.Errorf("not the latest: %s", st.OrderRef)
	}
}
