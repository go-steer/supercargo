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
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/backend"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/intents"
)

// backOfficeServices is Serra with its systems behind the node: the TMS
// (CT-e and MDF-e), the risk manager and the toll vouchers.
func backOfficeServices() (*RoadBookingService, *store.Bookings) {
	bookings, t := store.NewBookings(), trucks()
	t.CTe.MDFe, t.TMS = true, "TMS (simulated)"
	t.Risk = &MockRisk{Source: "Risk (simulated)", Refuse: "fireworks"}
	t.Vouchers = &MockVouchers{Source: "Vouchers (simulated)"}
	return &RoadBookingService{Bookings: bookings, Backend: t, Now: func() time.Time { return roadNow }}, bookings
}

// TestBackOffice: a leg is cleared, its CT-e and MDF-e issued and its toll
// voucher too; the shipper gets the documents' keys, and the carrier keeps
// what its systems did (the driver and vehicle never leave it).
func TestBackOffice(t *testing.T) {
	svc, bookings := backOfficeServices()
	reply, err := svc.Handle(context.Background(), roadRequest(meridian, roadBody()))
	if err != nil {
		t.Fatal(err)
	}
	conf := reply.Body.(*intents.RoadBookingConfirmationBody)
	if conf.Status != intents.StatusConfirmed || len(conf.FiscalDocuments) != 2 || conf.FiscalDocuments[1].Type != "MDF-e" ||
		conf.FiscalDocuments[1].AccessKey[20:22] != "58" || conf.FiscalDocuments[0].AccessKey[20:22] != "57" {
		t.Fatalf("confirmation %+v", conf)
	}
	if err := conf.Validate(); err != nil {
		t.Errorf("the confirmation doesn't validate: %v", err)
	}
	raw, _ := json.Marshal(conf)
	for _, private := range []string{"SM-", "CPF", "driver", "VP-", "simulated"} {
		if strings.Contains(string(raw), private) {
			t.Errorf("the shipper would see %q: %s", private, raw)
		}
	}
	b, _ := bookings.Get(serra, conf.BookingRef)
	steps := b.Road.BackOffice
	if len(steps) != 4 || steps[0].System != "risk" || steps[1].System != "tms" || steps[2].Reference != conf.FiscalDocuments[1].AccessKey || steps[3].System != "tolls" {
		t.Fatalf("steps %+v", steps)
	}
	if !regexp.MustCompile(`^SM-2026-[0-9]{6}$`).MatchString(steps[0].Reference) || !regexp.MustCompile(`^[A-Z]{3}[0-9][A-Z][0-9]{2} · driver `).MatchString(steps[0].Detail) ||
		steps[0].Source != "Risk (simulated)" || steps[1].Source != "TMS (simulated)" || !strings.HasPrefix(steps[3].Reference, "VP-") {
		t.Errorf("steps %+v", steps)
	}
	// Taken with the booking: not kept twice.
	if left := svc.Backend.(*TruckBookings).BackOffice(conf.BookingRef); left != nil {
		t.Errorf("kept after storing: %+v", left)
	}
}

// TestRiskRefused: a leg its risk manager doesn't clear is refused, its
// reason kept from the shipper; nothing is stored or issued.
func TestRiskRefused(t *testing.T) {
	svc, bookings := backOfficeServices()
	body := roadBody()
	body.Cargo.Description = "Fireworks"
	reply, err := svc.Handle(context.Background(), roadRequest(meridian, body))
	if err != nil {
		t.Fatal(err)
	}
	conf := reply.Body.(*intents.RoadBookingConfirmationBody)
	if conf.Status != intents.StatusRejected || conf.Messages[0].Code != "risk_not_cleared" || len(conf.FiscalDocuments) != 0 || len(bookings.List()) != 0 {
		t.Errorf("refused: %+v", conf)
	}
}

type failing struct{}

func (failing) Clear(context.Context, backend.RiskRequest) (*backend.Clearance, error) {
	return nil, errors.New("risk down")
}
func (failing) Issue(context.Context, backend.VoucherRequest) (*backend.Voucher, error) {
	return nil, errors.New("vouchers down")
}

// TestBackOfficeDown: a system that fails fails the booking (no reply, no
// leg): it isn't accepted unchecked.
func TestBackOfficeDown(t *testing.T) {
	for name, set := range map[string]func(*TruckBookings){
		"risk":     func(t *TruckBookings) { t.Risk = failing{} },
		"vouchers": func(t *TruckBookings) { t.Vouchers = failing{} },
	} {
		svc, bookings := backOfficeServices()
		set(svc.Backend.(*TruckBookings))
		if _, err := svc.Handle(context.Background(), roadRequest(meridian, roadBody())); err == nil || len(bookings.List()) != 0 {
			t.Errorf("%s down: %v, %d stored", name, err, len(bookings.List()))
		}
	}
}

// TestBackOfficeRoute: the control API shows a leg's steps to the
// carrier's people; a booking without any shows none.
func TestBackOfficeRoute(t *testing.T) {
	svc, bookings := backOfficeServices()
	reply, _ := svc.Handle(context.Background(), roadRequest(meridian, roadBody()))
	ref := reply.BookingRef
	_ = bookings.Add(store.Booking{Ref: "OB-1", Operator: serra})
	h := ControlHandler(nil, bookings, store.NewEventLog(10))
	get := func(path string) (int, BackOfficeResponse) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		var out BackOfficeResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	if code, out := get("/bookings/" + ref + "/backoffice"); code != 200 || len(out.Steps) != 4 {
		t.Errorf("HTTP %d %+v", code, out)
	}
	if code, out := get("/bookings/OB-1/backoffice"); code != 200 || out.Steps == nil || len(out.Steps) != 0 {
		t.Errorf("none: HTTP %d %+v", code, out)
	}
	if code, _ := get("/bookings/NOPE/backoffice"); code != 404 {
		t.Errorf("unknown: HTTP %d", code)
	}
}

// TestMocks: the mocks' answers have the real shapes, and are the same
// for the same booking.
func TestMocks(t *testing.T) {
	r := &MockRisk{Source: "x"}
	a, _ := r.Clear(context.Background(), backend.RiskRequest{BookingRef: "SR-BKG-0913", Pickup: roadNow})
	b, _ := r.Clear(context.Background(), backend.RiskRequest{BookingRef: "SR-BKG-0913", Pickup: roadNow})
	c, _ := r.Clear(context.Background(), backend.RiskRequest{BookingRef: "SR-BKG-0914", Pickup: roadNow})
	if *a != *b || a.Protocol == c.Protocol || !a.Approved {
		t.Errorf("%+v %+v %+v", a, b, c)
	}
	for i := range 200 {
		if p := plate(uint32(i) * 2654435761); !regexp.MustCompile(`^[A-Z]{3}[0-9][A-Z][0-9]{2}$`).MatchString(p) {
			t.Fatalf("plate %q", p)
		}
	}
	v, _ := (&MockVouchers{}).Issue(context.Background(), backend.VoucherRequest{BookingRef: "SR-BKG-0913"})
	if !regexp.MustCompile(`^VP-[0-9]{10}$`).MatchString(v.Number) {
		t.Errorf("voucher %q", v.Number)
	}
}
