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

package assistant

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/backend"
	"github.com/go-steer/supercargo/internal/business"
	"github.com/go-steer/supercargo/internal/docks"
)

// TestControlDocks: the dock schedule over the control API, the node's
// refusals carried as NodeErrors.
func TestControlDocks(t *testing.T) {
	m := &docks.Mock{Source: "s", Site: "DC", Docks: []string{"Dock 1"}, Zone: time.UTC, Now: func() time.Time { return thursday }}
	_ = m.Seed(&business.Context{Shipments: []business.Shipment{{ShipperRef: "CY-1",
		ReceivingSlot: &business.Slot{Date: thursday.AddDate(0, 0, 3).Format(time.DateOnly), Window: "06:00-10:00"}}}})
	mux := http.NewServeMux()
	docks.RegisterControl(mux, m)
	n := controlNode(t, mux)
	ctx := context.Background()
	a, err := n.DockAppointment(ctx, "CY-1")
	if err != nil || a.ShipperRef != "CY-1" {
		t.Fatalf("%+v %v", a, err)
	}
	var ne *NodeError
	if _, err := n.DockAppointment(ctx, "CY-9"); !errors.As(err, &ne) || ne.Status != http.StatusNotFound {
		t.Errorf("unknown: %v", err)
	}
	s, err := n.DockSlots(ctx, thursday.AddDate(0, 0, 5), 2)
	if err != nil || len(s) == 0 {
		t.Fatalf("%v %v", s, err)
	}
	if _, err := n.DockSlots(ctx, thursday, 30); !errors.As(err, &ne) || ne.Status != http.StatusBadRequest {
		t.Errorf("30 days: %v", err)
	}
	if moved, err := n.MoveDock(ctx, a.ID, a.Start, s[0]); err != nil || !moved.Start.Equal(s[0].Start) {
		t.Errorf("move: %+v %v", moved, err)
	}
	if _, err := n.MoveDock(ctx, a.ID, s[0].Start, backend.DockSlot{Dock: "Dock 9", Start: s[0].Start, End: s[0].End}); !errors.As(err, &ne) || ne.Status != http.StatusConflict {
		t.Errorf("a taken slot: %v", err)
	}
}
