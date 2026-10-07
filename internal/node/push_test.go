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

package node_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/internal/operator"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
	"github.com/go-steer/supercargo/pkg/transport/rest"
)

// booked books with the default subscription (events and alerts to the
// shipper's own /ulp) and returns the operator's record of the booking.
func booked(t *testing.T, d *demo, mutate func(*intents.BookingRequestBody)) store.Booking {
	t.Helper()
	req := bookingRequest()
	if mutate != nil {
		mutate(req)
	}
	conf, err := d.ship.Book(context.Background(), operatorDomain, req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := d.bookings.Get(operatorDomain, conf.BookingRef)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func event(id, code string) *intents.TnTEventBody {
	typ := intents.EventTypeEquipment
	e := &intents.TnTEventBody{
		EventID: id, EventType: typ, EventClassifierCode: intents.ClassifierActual, EventTypeCode: code,
		EventDateTime: "2026-10-07T11:20:00Z", UNLocationCode: "NLRTM", FacilityName: "Rotterdam", Description: "An event",
		EquipmentReference: "OCBU0000004",
	}
	if code == "DEPA" || code == "ARRI" {
		e.EventType, e.EquipmentReference = intents.EventTypeTransport, ""
		e.VesselName, e.VoyageNumber = "Ocean Blue Aurora", "42W"
	}
	return e
}

func alert(id string) *intents.TnTAlertBody {
	return &intents.TnTAlertBody{AlertID: id, Kind: intents.AlertDelay, Severity: intents.SeverityHigh,
		Summary: "Unscheduled call at Halifax for engine repair", PreviousETA: "2026-10-13T08:00:00Z",
		NewETA: "2026-10-16T14:00:00Z", CauseEventIDs: []string{"EV-1"}}
}

func waitRecords(t *testing.T, log *store.EventLog, operator, ref string, n int) []store.Record {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		recs := log.List(operator, ref)
		if len(recs) >= n {
			return recs
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d records after 5s, want %d", len(recs), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestEventsFlow: the operator publishes; the shipper verifies, authorizes
// and records each push, in order, once.
func TestEventsFlow(t *testing.T) {
	d := newDemo(t)
	b := booked(t, d, nil)
	var alerts atomic.Int32
	d.inbound.OnAlert = func(store.Booking, *intents.TnTAlertBody) { alerts.Add(1) }

	codes := []string{"GTIN", "LOAD", "DEPA", "ARRI"}
	for i, c := range codes {
		if err := d.publisher.Publish(b, event(fmt.Sprintf("EV-%d", i+1), c)); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.publisher.Publish(b, alert("AL-1")); err != nil {
		t.Fatal(err)
	}
	recs := waitRecords(t, d.shipLog, operatorDomain, b.Ref, 5)
	for i, c := range codes {
		if recs[i].ID != fmt.Sprintf("EV-%d", i+1) || !strings.Contains(string(recs[i].Body), c) || recs[i].Header.Sender != operatorDomain {
			t.Errorf("record %d: %+v", i, recs[i])
		}
	}
	if recs[4].Intent != intents.TnTAlert || alerts.Load() != 1 {
		t.Errorf("alert record %+v, OnAlert ran %d times", recs[4], alerts.Load())
	}
	if n := len(d.opLog.List(operatorDomain, b.Ref)); n != 5 {
		t.Errorf("operator logged %d, want 5", n)
	}
	if e := d.shipper.lastIn(t); e.Outcome != "accepted" || !e.Verified || e.Sender != operatorDomain {
		t.Errorf("shipper's last event: %+v", e)
	}

	// The same ID published again is refused, and nothing more arrives.
	if err := d.publisher.Publish(b, event("EV-1", "GTIN")); !errors.Is(err, operator.ErrDuplicate) {
		t.Fatalf("republishing EV-1: %v, want ErrDuplicate", err)
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(d.shipLog.List(operatorDomain, b.Ref)); n != 5 {
		t.Errorf("a duplicate event was recorded: %d records", n)
	}
	if len(d.drops()) != 0 {
		t.Errorf("drops: %v", d.drops())
	}
}

// TestPushOnlyWhatWasAskedFor: no subscription, or one without alerts,
// means nothing (or no alerts) is pushed; the operator still records them.
func TestPushOnlyWhatWasAskedFor(t *testing.T) {
	d := newDemo(t)
	noSub := booked(t, d, func(r *intents.BookingRequestBody) { r.Subscription = nil })
	eventsOnly := booked(t, d, func(r *intents.BookingRequestBody) { r.Subscription.Intents = []string{intents.TnTEvent} })
	_ = d.publisher.Publish(noSub, event("EV-1", "GTIN"))
	_ = d.publisher.Publish(eventsOnly, alert("AL-1"))
	_ = d.publisher.Publish(eventsOnly, event("EV-2", "GTIN"))
	waitRecords(t, d.shipLog, operatorDomain, eventsOnly.Ref, 1)
	time.Sleep(100 * time.Millisecond)
	if got := d.shipLog.List(operatorDomain, noSub.Ref); len(got) != 0 {
		t.Errorf("pushed to a booking without a subscription: %v", got)
	}
	if got := d.shipLog.List(operatorDomain, eventsOnly.Ref); len(got) != 1 || got[0].ID != "EV-2" {
		t.Errorf("events-only subscription received %v", got)
	}
	if len(d.opLog.List(operatorDomain, noSub.Ref)) != 1 || len(d.opLog.List(operatorDomain, eventsOnly.Ref)) != 2 {
		t.Error("the operator didn't record everything it published")
	}
}

// TestInboundAuthorization: the shipper accepts pushes only from the
// booking's operator, for a subscribed intent.
func TestInboundAuthorization(t *testing.T) {
	d := newDemo(t)
	b := booked(t, d, func(r *intents.BookingRequestBody) { r.Subscription.Intents = []string{intents.TnTEvent} })
	noSub := booked(t, d, func(r *intents.BookingRequestBody) { r.Subscription = nil })
	endpoint := "https://" + shipperDomain + "/ulp"
	push := func(from *company, ref string, body intents.Body) error {
		h := envelope.NewHeader(body.Intent(), from.domain, shipperDomain, time.Now())
		h.BookingRef = &ref
		return from.node.Push(context.Background(), h, body, endpoint)
	}
	for name, err := range map[string]error{
		"another carrier pushing to the operator's booking": push(d.rival, b.Ref, event("EV-9", "GTIN")),
		"the operator, an unsubscribed intent":              push(d.operator, b.Ref, alert("AL-9")),
		"the operator, a booking without a subscription":    push(d.operator, noSub.Ref, event("EV-9", "GTIN")),
		"the operator, an unknown booking":                  push(d.operator, "OB-BKG-9999", event("EV-9", "GTIN")),
	} {
		var re *node.RemoteError
		if !errors.As(err, &re) || re.Code != intents.CodeForbidden || re.Retryable {
			t.Errorf("%s: %v, want a non-retryable forbidden RemoteError", name, err)
		}
	}
	if n := len(d.shipLog.List(operatorDomain, b.Ref)) + len(d.shipLog.List("other-carrier.example", b.Ref)); n != 0 {
		t.Errorf("%d unauthorized pushes recorded", n)
	}
	// Mallory can't even be verified: a bare refusal.
	var se *rest.StatusError
	if err := push(d.mallory, b.Ref, event("EV-9", "GTIN")); !errors.As(err, &se) {
		t.Errorf("unverifiable sender: %v, want a bare status", err)
	}
}

// TestPushRetries: a subscriber that is briefly unavailable gets the push
// once it recovers; one that refuses outright is given up on.
func TestPushRetries(t *testing.T) {
	d := newDemo(t)
	b := booked(t, d, nil)
	cy := d.shipper
	var failures atomic.Int32
	failures.Store(2)
	cy.mu.Lock()
	real := cy.handler
	cy.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && failures.Add(-1) >= 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		real.ServeHTTP(w, r)
	})
	cy.mu.Unlock()
	if err := d.publisher.Publish(b, event("EV-1", "GTIN")); err != nil {
		t.Fatal(err)
	}
	waitRecords(t, d.shipLog, operatorDomain, b.Ref, 1)
	if len(d.drops()) != 0 {
		t.Errorf("a push that succeeded on retry was reported dropped: %v", d.drops())
	}

	// A subscriber that refuses outright (a bare 403) isn't retried.
	var posts atomic.Int32
	cy.mu.Lock()
	cy.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		real.ServeHTTP(w, r)
	})
	cy.mu.Unlock()
	_ = d.publisher.Publish(b, event("EV-2", "LOAD"))
	deadline := time.Now().Add(5 * time.Second)
	for len(d.drops()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if drops := d.drops(); len(drops) != 1 || !strings.HasPrefix(drops[0], "EV-2") {
		t.Errorf("drops: %v", drops)
	}
	if posts.Load() != 1 {
		t.Errorf("a refused push was attempted %d times, want 1", posts.Load())
	}
}

// TestPushEndpointMustBeTheReceivers: Push never posts outside the
// receiver's domain, whatever endpoint it is handed.
func TestPushEndpointMustBeTheReceivers(t *testing.T) {
	d := newDemo(t)
	b := booked(t, d, nil)
	before := d.shipper.inCount()
	for endpoint, want := range map[string]string{
		"https://" + operatorDomain + "/ulp":     "is not on",
		"https://" + shipperDomain + ":8443/ulp": "is not on",
		"https://user@" + shipperDomain + "/ulp": "is not on",
		"http://" + shipperDomain + "/ulp":       "is not https",
	} {
		h := envelope.NewHeader(intents.TnTEvent, operatorDomain, shipperDomain, time.Now())
		ref := b.Ref
		h.BookingRef = &ref
		if err := d.operator.node.Push(context.Background(), h, event("EV-7", "GTIN"), endpoint); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Push to %s = %v, want %q", endpoint, err, want)
		}
	}
	if n := d.shipper.inCount() - before; n != 0 {
		t.Errorf("the shipper's node saw %d inbound messages", n)
	}
}
