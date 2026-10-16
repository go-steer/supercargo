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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/backend"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/intents"
)

type answers struct {
	c *backend.Clearance
	v *backend.Voucher
}

func (a answers) Clear(context.Context, backend.RiskRequest) (*backend.Clearance, error) {
	return a.c, nil
}
func (a answers) Issue(context.Context, backend.VoucherRequest) (*backend.Voucher, error) {
	return a.v, nil
}

// TestBackOfficeAnswersChecked: a system's missing or malformed answer
// fails the booking (nothing stored, nothing left behind), never kept.
func TestBackOfficeAnswersChecked(t *testing.T) {
	good := backend.Voucher{Number: "VP-1", Source: "v"}
	for name, a := range map[string]answers{
		"no clearance":           {nil, &good},
		"approved, no protocol":  {&backend.Clearance{Approved: true}, &good},
		"a bidi in the driver":   {&backend.Clearance{Approved: true, Protocol: "SM-1", Driver: "a\u202eb"}, &good},
		"a long vehicle":         {&backend.Clearance{Approved: true, Protocol: "SM-1", Vehicle: strings.Repeat("x", 129)}, &good},
		"no voucher":             {&backend.Clearance{Approved: true, Protocol: "SM-1"}, nil},
		"a voucher without one":  {&backend.Clearance{Approved: true, Protocol: "SM-1"}, &backend.Voucher{Source: "v"}},
		"a voucher with a break": {&backend.Clearance{Approved: true, Protocol: "SM-1"}, &backend.Voucher{Number: "VP\n1"}},
	} {
		svc, bookings := backOfficeServices()
		tb := svc.Backend.(*TruckBookings)
		tb.Risk, tb.Vouchers = a, a
		if _, err := svc.Handle(context.Background(), roadRequest(meridian, roadBody())); err == nil || len(bookings.List()) != 0 {
			t.Errorf("%s: %v, %d stored", name, err, len(bookings.List()))
		}
		if len(tb.steps) != 0 {
			t.Errorf("%s: steps left behind: %v", name, tb.steps)
		}
	}
}

// TestBackOfficeFull: a leg the store can't take is refused, and its
// steps aren't left behind.
func TestBackOfficeFull(t *testing.T) {
	svc, bookings := backOfficeServices()
	for i := range store.MaxBookings {
		if err := bookings.Add(store.Booking{Ref: fmt.Sprint("X-", i), Operator: serra}); err != nil {
			t.Fatal(err)
		}
	}
	reply, err := svc.Handle(context.Background(), roadRequest(meridian, roadBody()))
	if err != nil || reply.Body.(*intents.RoadBookingConfirmationBody).Status != intents.StatusRejected {
		t.Fatalf("%+v %v", reply, err)
	}
	if n := len(svc.Backend.(*TruckBookings).steps); n != 0 {
		t.Errorf("%d steps left behind", n)
	}
}

// TestStepsBound: steps never taken are bounded, the oldest dropped.
func TestStepsBound(t *testing.T) {
	tb := trucks()
	for i := range maxSteps + 5 {
		tb.record(fmt.Sprint("SR-", i), []store.BackOfficeStep{{At: roadNow.Add(time.Duration(i) * time.Second)}})
	}
	if _, oldest := tb.steps["SR-0"]; len(tb.steps) != maxSteps || oldest {
		t.Errorf("%d kept, the oldest %v", len(tb.steps), oldest)
	}
}

// TestBackOfficeKept: an amendment keeps the leg's back office; the
// carrier's bookings summary never shows it.
func TestBackOfficeKept(t *testing.T) {
	svc, bookings := backOfficeServices()
	reply, _ := svc.Handle(context.Background(), roadRequest(meridian, roadBody()))
	ref := reply.BookingRef
	am := &RoadAmendmentService{Bookings: bookings, Backend: svc.Backend.(*TruckBookings)}
	if _, err := am.Handle(context.Background(), amendment(meridian, ref, &intents.RoadBookingAmendmentBody{
		Pickup: &intents.Window{Earliest: "2026-10-26T14:00:00Z"}, Delivery: &intents.Window{Latest: "2026-10-30T12:00:00Z"}})); err != nil {
		t.Fatal(err)
	}
	b, _ := bookings.Get(serra, ref)
	if len(b.Road.BackOffice) != 4 || b.Road.Confirmation.PlannedPickup != "2026-10-26T14:00:00Z" {
		t.Errorf("after the amendment: %+v", b.Road)
	}
	rec := httptest.NewRecorder()
	ControlHandler(nil, bookings, store.NewEventLog(10)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/bookings", nil))
	for _, private := range []string{"SM-", "CPF", "VP-", "simulated"} {
		if strings.Contains(rec.Body.String(), private) {
			t.Errorf("the summary shows %q", private)
		}
	}
}
