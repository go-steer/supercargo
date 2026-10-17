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

package app

import (
	"context"
	"time"

	"github.com/go-steer/supercargo/internal/activity"
	"github.com/go-steer/supercargo/internal/docks"
	"github.com/go-steer/supercargo/internal/store"
)

// Dock policy moments.
const (
	dockGranted = iota // the subscription confirmed the leg: reserve
	dockAlerted        // a verified alert gave a new arrival: follow
)

// keepDock applies the site's standing dock policy to a truck it follows
// by delegation (by its carrier and reference), arriving at arrival (an
// RFC 3339 time from the leg's plan or a verified alert), and says what
// it did in the activity stream. A grant reserves a slot and never moves
// one (a newer arrival may already have); an alert moves it. The
// appointment is the site's own: nothing goes in an envelope.
func (a *App) keepDock(b store.Booking, arrival string, moment int) {
	p := a.DockPolicy
	if p == nil || b.Delegated == nil {
		return
	}
	eta, err := time.Parse(time.RFC3339, arrival)
	if err != nil {
		return // no arrival known yet
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	key, issuer, until := docks.TruckKey(b.Operator, b.Ref), b.Delegated.Issuer, b.Delegated.Expires
	var o *docks.Outcome
	if moment == dockGranted {
		o, err = p.Reserve(ctx, key, issuer, until, eta)
	} else {
		o, err = p.Follow(ctx, key, issuer, until, eta)
	}
	d := &activity.Dock{Booking: b.Ref, Arrival: arrival, Policy: p.Note()}
	if err != nil {
		d.Action, d.Error = "unavailable", err.Error()
	} else {
		ap := o.Appointment
		d.Action, d.Dock, d.Start, d.End, d.FromDock, d.FromStart = o.Action, ap.Dock, ap.Start, ap.End, o.FromDock, o.FromStart
	}
	a.Activity.Publish(activity.Entry{Side: activity.SideCustomer, Kind: activity.KindDock, Dock: d})
}

// plannedArrival is a booking's planned arrival or delivery, as planned.
func plannedArrival(b store.Booking) string {
	if b.Road != nil {
		return b.Road.Confirmation.PlannedDelivery
	}
	return b.Confirmation.PlannedArrival
}
