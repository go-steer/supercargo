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
	"errors"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
)

func service() *BookingService {
	return &BookingService{
		Bookings: store.NewBookings(), RefPrefix: "OB-BKG", ContainerPrefix: "OCBU", Now: time.Now,
		Deny:   map[string]bool{"blocked.example": true},
		Voyage: Voyage{Vessel: "Ocean Blue Aurora", Number: "42W", DepartureHour: 18, Transit: 134 * time.Hour},
	}
}

func request(sender string, b *intents.BookingRequestBody) *node.Request {
	h := envelope.NewHeader(intents.BookingRequest, sender, "ocean-blue.example", time.Now())
	return &node.Request{Envelope: &envelope.Envelope{Header: h}, Body: b}
}

func body() *intents.BookingRequestBody {
	return &intents.BookingRequestBody{
		ShipperRef: "P1", EarliestDeparture: "2026-10-06",
		ShipperName:     "Cymbal Shoes",
		PortOfLoading:   "NLRTM",
		PortOfDischarge: "USEWR",
		Equipment:       []intents.Equipment{{ISOType: "45G1", Count: 2, GrossWeightKg: 12000}, {ISOType: "22G1", Count: 1, GrossWeightKg: 12000}},
		Cargo:           intents.Cargo{Description: "Shoes", HSCode: "6404"},
	}
}

func TestAuthorize(t *testing.T) {
	s := service()
	if err := s.Authorize(context.Background(), request("cymbal-shoes.example", body())); err != nil {
		t.Errorf("plain request: %v", err)
	}
	b := body()
	b.Subscription = &intents.Subscription{Endpoint: "https://cymbal-shoes.example/ulp", Intents: []string{intents.TnTEvent}}
	if err := s.Authorize(context.Background(), request("cymbal-shoes.example", b)); err != nil {
		t.Errorf("subscription on the sender's domain: %v", err)
	}
	for name, req := range map[string]*node.Request{
		"denied sender": request("blocked.example", body()),
		"subscription on another domain": func() *node.Request {
			b := body()
			b.Subscription = &intents.Subscription{Endpoint: "https://victim.example/ulp", Intents: []string{intents.TnTEvent}}
			return request("cymbal-shoes.example", b)
		}(),
		"subscription on a subdomain": func() *node.Request {
			b := body()
			b.Subscription = &intents.Subscription{Endpoint: "https://hooks.cymbal-shoes.example/ulp", Intents: []string{intents.TnTEvent}}
			return request("cymbal-shoes.example", b)
		}(),
	} {
		err := s.Authorize(context.Background(), req)
		var r *node.Rejection
		if !errors.As(err, &r) || r.Code != intents.CodeForbidden {
			t.Errorf("%s: %v, want a forbidden Rejection", name, err)
		}
	}
}

func TestCheckDigit(t *testing.T) {
	// ISO 6346's own example: CSQU305438, check digit 3.
	if d := checkDigit("CSQU3054383"); d != 3 {
		t.Errorf("checkDigit(CSQU305438) = %d, want 3", d)
	}
	if n := containerNumber("CSQU", 305438); n != "CSQU3054383" {
		t.Errorf("containerNumber = %s", n)
	}
}

// TestContainersAreUnique: serials come from one counter, so no two
// containers share a number, within a booking or across bookings.
func TestContainersAreUnique(t *testing.T) {
	s := service()
	seen := map[string]bool{}
	for range 5 {
		b := body()
		b.Equipment = []intents.Equipment{{ISOType: "45G1", Count: 30, GrossWeightKg: 12000}, {ISOType: "22G1", Count: 20, GrossWeightKg: 12000}}
		reply, err := s.Handle(context.Background(), request("cymbal-shoes.example", b))
		if err != nil {
			t.Fatal(err)
		}
		c := reply.Body.(*intents.BookingConfirmationBody)
		if err := c.Validate(); err != nil {
			t.Fatalf("confirmation doesn't validate: %v", err)
		}
		for _, r := range c.EquipmentRefs() {
			if seen[r] {
				t.Fatalf("container %s assigned twice", r)
			}
			if checkDigit(r) != int(r[10]-'0') {
				t.Errorf("container %s has a wrong check digit", r)
			}
			seen[r] = true
		}
	}
	if len(seen) != 5*50 {
		t.Errorf("%d containers, want 250", len(seen))
	}
}

// TestTooManyContainersRejected: a request over the cap is answered
// "rejected", not booked, and consumes no serials.
func TestTooManyContainersRejected(t *testing.T) {
	s := service()
	b := body()
	b.Equipment = []intents.Equipment{{ISOType: "45G1", Count: MaxContainers, GrossWeightKg: 12000}, {ISOType: "22G1", Count: 1, GrossWeightKg: 12000}}
	reply, err := s.Handle(context.Background(), request("cymbal-shoes.example", b))
	if err != nil {
		t.Fatal(err)
	}
	c := reply.Body.(*intents.BookingConfirmationBody)
	if c.Status != intents.StatusRejected || len(c.EquipmentRefs()) != 0 || len(c.Messages) != 1 || c.Validate() != nil {
		t.Errorf("over-cap answer %+v", c)
	}
	if s.Bookings.Len() != 0 {
		t.Error("an over-cap request was booked")
	}
	reply, _ = s.Handle(context.Background(), request("cymbal-shoes.example", body()))
	if refs := reply.Body.(*intents.BookingConfirmationBody).EquipmentRefs(); refs[0] != containerNumber("OCBU", 0) {
		t.Errorf("serials were consumed by the rejected request: first is %s", refs[0])
	}
}

// TestFullStoreRejected: once the booking store is full, a request is
// answered "rejected" rather than failing.
func TestFullStoreRejected(t *testing.T) {
	s := service()
	s.Bookings = store.NewBookingsCap(1)
	if _, err := s.Handle(context.Background(), request("cymbal-shoes.example", body())); err != nil {
		t.Fatal(err)
	}
	reply, err := s.Handle(context.Background(), request("cymbal-shoes.example", body()))
	if err != nil {
		t.Fatalf("a full store failed the request: %v", err)
	}
	c := reply.Body.(*intents.BookingConfirmationBody)
	if c.Status != intents.StatusRejected || len(c.Messages) != 1 || c.Messages[0].Code != "at_capacity" || c.Validate() != nil {
		t.Errorf("full-store answer %+v", c)
	}
	if s.Bookings.Len() != 1 {
		t.Errorf("%d bookings, want 1", s.Bookings.Len())
	}
}

func TestHandle(t *testing.T) {
	s := service()
	reply, err := s.Handle(context.Background(), request("cymbal-shoes.example", body()))
	if err != nil {
		t.Fatal(err)
	}
	c := reply.Body.(*intents.BookingConfirmationBody)
	if err := c.Validate(); err != nil {
		t.Errorf("confirmation doesn't validate: %v", err)
	}
	if reply.BookingRef != c.BookingRef || c.BookingRef != "OB-BKG-1042" || len(c.EquipmentRefs()) != 3 || c.SubscriptionID != "" {
		t.Errorf("confirmation %+v", c)
	}
	seen := map[string]bool{}
	for _, r := range c.EquipmentRefs() {
		if seen[r] {
			t.Errorf("container %s assigned twice", r)
		}
		seen[r] = true
	}
	if b, err := s.Bookings.Get("ocean-blue.example", c.BookingRef); err != nil || b.Shipper != "cymbal-shoes.example" {
		t.Errorf("stored %+v, %v", b, err)
	}
	reply, _ = s.Handle(context.Background(), request("cymbal-shoes.example", body()))
	if reply.BookingRef != "OB-BKG-1043" {
		t.Errorf("second booking %s", reply.BookingRef)
	}
	bad := body()
	bad.EarliestDeparture = "soon"
	if _, err := s.Handle(context.Background(), request("cymbal-shoes.example", bad)); err == nil {
		t.Error("Handle accepted an unparseable date")
	}
}
