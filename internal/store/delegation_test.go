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
	"testing"
	"time"

	"github.com/go-steer/supercargo/pkg/intents"
)

// TestDelegationRecords: a delegate and a delegated booking hold until
// their grant's end; a booking's copies don't share them.
func TestDelegationRecords(t *testing.T) {
	end := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	d := Delegate{Expires: end}
	if !d.Active(end.Add(-time.Second)) || d.Active(end) {
		t.Error("Delegate.Active")
	}
	db := &DelegatedBooking{Expires: end}
	var none *DelegatedBooking
	if !db.Active(end.Add(-time.Second)) || db.Active(end) || none.Active(end.Add(-time.Hour)) {
		t.Error("DelegatedBooking.Active")
	}
	s := NewBookings()
	if err := s.Add(Booking{Ref: "R-1", Operator: "c.example",
		Delegates: []Delegate{{Grantee: "g.example", Subscription: intents.Subscription{Intents: []string{intents.TnTEvent}}, Expires: end}},
		Delegated: &DelegatedBooking{Issuer: "i.example", Scope: []string{"tnt:read"}, Expires: end},
		Grants:    []IssuedGrant{{Grantee: "g.example", Scope: []string{"tnt:read"}, Expires: end}},
	}); err != nil {
		t.Fatal(err)
	}
	a, _ := s.Get("c.example", "R-1")
	a.Delegates[0].Subscription.Intents[0] = "x"
	a.Delegated.Scope[0] = "x"
	a.Grants[0].Scope[0] = "x"
	b, _ := s.Get("c.example", "R-1")
	if b.Delegates[0].Subscription.Intents[0] != intents.TnTEvent || b.Delegated.Scope[0] != "tnt:read" || b.Grants[0].Scope[0] != "tnt:read" {
		t.Errorf("copies share: %+v", b)
	}
}

// TestSummaryDelegation: a delegated booking says by whom and until when
// (no kind before its leg is known); a shared one, with whom.
func TestSummaryDelegation(t *testing.T) {
	end := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	s := Summarize(Booking{Ref: "R-1", Operator: "c.example", Shipper: "i.example", Delegated: &DelegatedBooking{Issuer: "i.example", Expires: end}}, nil)
	if s.DelegatedBy != "i.example" || s.DelegatedUntil != "2026-11-01T00:00:00Z" || s.Kind != "" {
		t.Errorf("%+v", s)
	}
	s = Summarize(Booking{Ref: "R-2", Operator: "c.example", Grants: []IssuedGrant{{Grantee: "g.example"}, {Grantee: "g.example"}, {Grantee: "h.example"}}}, nil)
	if len(s.SharedWith) != 2 || s.SharedWith[0] != "g.example" {
		t.Errorf("%+v", s.SharedWith)
	}
}
