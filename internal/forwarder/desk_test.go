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
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/intents"
)

// TestDismissProposal: a dismissed proposal is closed without sending
// anything; it can't be confirmed or dismissed again.
func TestDismissProposal(t *testing.T) {
	r, _, am := relayed(t)
	_ = r.Event(shipLeg, event("EV-1", "est"))
	_ = r.Alert(shipLeg, delay("AL-1", "EV-1", "2026-10-23T14:00:00Z", "2026-10-26T14:00:00Z"))
	id := r.Proposals("MF-0007")[0].ID
	p, err := r.Dismiss(id)
	if err != nil || p.Status != ProposalDismissed {
		t.Fatalf("dismiss: %+v %v", p, err)
	}
	if _, err := r.Dismiss(id); !errors.Is(err, ErrProposalClosed) {
		t.Errorf("dismissed again: %v", err)
	}
	if _, err := r.Confirm(context.Background(), id); !errors.Is(err, ErrProposalClosed) || am.got != nil {
		t.Errorf("confirmed after dismissing: %v, sent %v", err, am.got)
	}
	if _, err := r.Dismiss("MF-PR-999999"); !errors.Is(err, ErrUnknownProposal) {
		t.Errorf("unknown: %v", err)
	}
	// Superseded by a later delay, or being sent: closed to dismissal too.
	_ = r.Alert(shipLeg, delay("AL-3", "EV-1", "2026-10-26T14:00:00Z", "2026-10-27T14:00:00Z"))
	_ = r.Alert(shipLeg, delay("AL-4", "EV-1", "2026-10-27T14:00:00Z", "2026-10-28T14:00:00Z"))
	ps := r.Proposals("MF-0007")
	older, newer := ps[len(ps)-2], ps[len(ps)-1]
	if older.Status != ProposalSuperseded {
		t.Fatalf("not superseded: %+v", older)
	}
	if _, err := r.Dismiss(older.ID); !errors.Is(err, ErrProposalClosed) {
		t.Errorf("superseded: %v", err)
	}
	r.mu.Lock()
	r.proposals[newer.ID].Status = ProposalSending
	r.mu.Unlock()
	if _, err := r.Dismiss(newer.ID); !errors.Is(err, ErrProposalClosed) {
		t.Errorf("being sent: %v", err)
	}
	r.mu.Lock()
	r.proposals[newer.ID].Status = ProposalOpen
	r.mu.Unlock()
	// Through the control API.
	_ = r.Alert(shipLeg, delay("AL-2", "EV-1", "2026-10-26T14:00:00Z", "2026-10-28T14:00:00Z"))
	ps = r.Proposals("MF-0007")
	next := ps[len(ps)-1].ID
	mux := http.NewServeMux()
	RegisterControl(mux, r.Orders, nil, r, nil)
	for _, tc := range []struct {
		path string
		want int
	}{
		{"/proposals/" + next + "/dismiss", http.StatusOK},
		{"/proposals/" + next + "/dismiss", http.StatusConflict},
		{"/proposals/MF-PR-999999/dismiss", http.StatusNotFound},
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, tc.path, nil))
		if rec.Code != tc.want {
			t.Errorf("%s: HTTP %d, want %d", tc.path, rec.Code, tc.want)
		}
	}
	if am.got != nil {
		t.Errorf("a dismissal sent %+v", am.got)
	}
}

// TestRejectWithLegsBooked: an order whose booking stopped part way (the
// ship booked, the truck refused) isn't rejected by the order desk alone:
// the shipper would be told the order is off while a carrier holds its
// leg. The orchestrator's Reject cancels the legs first (TestReject*);
// without a way to cancel them, nothing is sent.
func TestRejectWithLegsBooked(t *testing.T) {
	orch, sent := orchestrated(t, &carriers{refuseRoad: true}, portToDoor)
	if _, err := orch.Orders.Handle(context.Background(), orderFrom(cymbal, doorOrder())); err != nil {
		t.Fatal(err)
	}
	if _, err := orch.Book(context.Background(), "MF-0007"); err == nil {
		t.Fatal("booked with the truck refusing")
	}
	before := len(sent.bodies)
	err := orch.Orders.Answer(context.Background(), "MF-0007", &intents.ForwardingConfirmationBody{Status: intents.StatusRejected})
	if !errors.Is(err, ErrLegsBooked) || len(sent.bodies) != before {
		t.Errorf("rejected with a leg booked: %v, sent %d", err, len(sent.bodies)-before)
	}
	if b, _ := orch.Orders.Bookings.Get(meridian, "MF-0007"); b.Order.Confirmation.Status != intents.StatusPending {
		t.Errorf("the order is %s", b.Order.Confirmation.Status)
	}
	mux := http.NewServeMux()
	RegisterControl(mux, orch.Orders, orch, nil, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/orders/answer", strings.NewReader(`{"orderRef":"MF-0007","answer":{"status":"rejected"}}`))
	req.Header.Set("Content-Type", "application/json")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway || len(sent.bodies) != before {
		t.Errorf("through the control API, with no way to cancel the leg: HTTP %d, sent %d", rec.Code, len(sent.bodies)-before)
	}
}

// TestControlOrder: one order as the desk shows it: what was ordered, the
// plan on its lane, the legs, the journey from the order's own records and
// the proposals; no such order is a 404.
func TestControlOrder(t *testing.T) {
	orch, _ := orchestrated(t, &carriers{}, portToDoor)
	if _, err := orch.Orders.Handle(context.Background(), orderFrom(cymbal, doorOrder())); err != nil {
		t.Fatal(err)
	}
	log := store.NewEventLog(0)
	mux := http.NewServeMux()
	RegisterControl(mux, orch.Orders, orch, nil, log)
	get := func(path string) (int, OrderResponse) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		var resp OrderResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		return rec.Code, resp
	}
	code, resp := get("/orders/MF-0007")
	v := resp.Order
	if code != http.StatusOK || v == nil || v.Ref != "MF-0007" || v.Shipper != cymbal || v.Answer.Status != intents.StatusPending ||
		v.Request.ShipperRef != doorOrder().ShipperRef || len(v.Steps) != 2 || v.Steps[0].Booked != "" || len(v.Legs) != 0 || v.Proposals == nil {
		t.Fatalf("pending: HTTP %d %+v", code, resp)
	}
	if code, _ := get("/orders/MF-9999"); code != http.StatusNotFound {
		t.Errorf("no such order: HTTP %d", code)
	}
	// A booking of the forwarder's that isn't an order: not an order.
	if err := orch.Orders.Bookings.Add(store.Booking{Ref: "MF-0001", Operator: meridian, Shipper: cymbal}); err != nil {
		t.Fatal(err)
	}
	if code, resp := get("/orders/MF-0001"); code != http.StatusNotFound || resp.Order != nil {
		t.Errorf("not an order: HTTP %d %+v", code, resp)
	}
	// Booked: the legs, the steps' references, and the order's journey
	// from the events the forwarder re-issued on it.
	if _, err := orch.Book(context.Background(), "MF-0007"); err != nil {
		t.Fatal(err)
	}
	b, _ := orch.Orders.Bookings.Get(meridian, "MF-0007")
	first := b.Order.Confirmation.Legs[0]
	dep := intents.TnTEventBody{EventID: "MF-EV-000001", EventType: intents.EventTypeTransport, EventTypeCode: "DEPA",
		EventClassifierCode: intents.ClassifierActual, EventDateTime: "2026-10-07T18:00:00Z", UNLocationCode: first.From}
	raw, _ := json.Marshal(dep)
	if _, err := log.Add(meridian, "MF-0007", store.Record{ID: dep.EventID, Intent: intents.TnTEvent, Body: raw, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	code, resp = get("/orders/MF-0007")
	v = resp.Order
	if code != http.StatusOK || v.Answer.Status != intents.StatusConfirmed || len(v.Legs) != 2 || v.Steps[0].Booked != v.Legs[0].Ref ||
		len(v.Journey.Legs) != 2 || v.Journey.Legs[0].Status != store.LegInProgress || v.Journey.Legs[0].Departure.Source != "MF-EV-000001" {
		t.Errorf("booked: HTTP %d %+v", code, v)
	}
	// No lane any more: the order still shows, saying why there's no plan.
	orch.Lanes = nil
	if code, resp := get("/orders/MF-0007"); code != http.StatusOK || resp.Order.PlanError == "" || len(resp.Order.Legs) != 2 {
		t.Errorf("no lane: HTTP %d %+v", code, resp.Order)
	}
}
