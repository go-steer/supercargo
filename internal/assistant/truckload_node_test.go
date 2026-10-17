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

	"github.com/go-steer/supercargo/internal/shipper"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/intents"
)

// TestControlTruckload: a truckload and a grant over the control API,
// refusals as NodeErrors.
func TestControlTruckload(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /book/road", func(w http.ResponseWriter, r *http.Request) {
		answer(w, http.StatusOK, shipper.RoadBookResponse{Confirmation: &intents.RoadBookingConfirmationBody{BookingRef: "SR-BKG-0913", Status: intents.StatusConfirmed}})
	})
	mux.HandleFunc("POST /shipments/{op}/{ref}/grant", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("ref") != "SR-BKG-0913" {
			answer(w, http.StatusNotFound, shipper.GrantResponse{Error: "not ours"})
			return
		}
		answer(w, http.StatusOK, shipper.GrantResponse{Grant: &store.IssuedGrant{Grantee: "mercado-aurora.example"}})
	})
	n := controlNode(t, mux)
	ctx := context.Background()
	if c, err := n.BookRoad(ctx, "serra-transportes.example", intents.RoadBookingRequestBody{}); err != nil || c.BookingRef != "SR-BKG-0913" {
		t.Errorf("%+v %v", c, err)
	}
	if g, err := n.Grant(ctx, "serra-transportes.example", "SR-BKG-0913", shipper.GrantRequest{}); err != nil || g.Grantee != "mercado-aurora.example" {
		t.Errorf("%+v %v", g, err)
	}
	var ne *NodeError
	if _, err := n.Grant(ctx, "serra-transportes.example", "SR-BKG-0001", shipper.GrantRequest{}); !errors.As(err, &ne) || ne.Status != http.StatusNotFound {
		t.Errorf("not ours: %v", err)
	}
	if _, err := controlNode(t, http.NewServeMux()).BookRoad(ctx, "x.example", intents.RoadBookingRequestBody{}); err == nil {
		t.Error("no route answered")
	}
}
