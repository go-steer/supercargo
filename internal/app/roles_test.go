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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/business"
	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/internal/shipper"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/discovery"
	"github.com/go-steer/supercargo/pkg/intents"
)

// sockDir is an owner-only directory for a control socket.
func sockDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if err := os.Chmod(d, 0o700); err != nil {
		t.Fatal(err)
	}
	return d
}

// controlGet reads path from a's control API into v.
func controlGet(t *testing.T, a *App, path string, v any) int {
	t.Helper()
	rec := httptest.NewRecorder()
	a.Control.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("%s: %v (%s)", path, err, rec.Body)
	}
	return rec.Code
}

// TestTwoRoleNode: Cymbal Shoes is a shipper and a carrier. It books with
// Ocean Blue as a shipper; its carrier side never sees that booking, and
// its profile and control API serve both roles.
func TestTwoRoleNode(t *testing.T) {
	p := startPairWith(t, "", `roles: [shipper, operator]
control: { socket: "`+sockDir(t)+`/cy.sock" }
operator: { refPrefix: CY-BKG, containerPrefix: CYMU, vessel: Sneaker, voyage: 1E, departureHour: 6, transit: 48h }`)
	cy := p.shipper
	if cy.Carrier == nil || cy.Customer == nil || cy.Carrier.Bookings == cy.Customer.Bookings || cy.Carrier.Events == cy.Customer.Events {
		t.Fatal("a two-role node doesn't keep each side's records apart")
	}
	conf, err := cy.Shipper.Book(context.Background(), "ocean-blue.example", request())
	if err != nil || conf.Status != intents.StatusConfirmed {
		t.Fatalf("Book: %+v, %v", conf, err)
	}

	// Its profile, as Ocean Blue fetched it, lists both roles and the
	// intents of both.
	doc, err := p.operator.Keys.Profile(context.Background(), "cymbal-shoes.example")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(doc.Profile.Party.Roles, []string{"shipper", "operator"}) || !doc.Accepts(intents.BookingRequest) || !doc.Accepts(intents.TnTEvent) {
		t.Errorf("profile roles %v", doc.Profile.Party.Roles)
	}

	// Ocean Blue asks Cymbal's carrier side about the booking Cymbal holds
	// as a shipper: the carrier side has no such booking, so the answer is
	// the same forbidden as for any unknown booking.
	h := intents.HeaderFor(intents.TnTInquiry, "ocean-blue.example", "cymbal-shoes.example", time.Now())
	h.BookingRef = &conf.BookingRef
	_, err = p.operator.Node.Exchange(context.Background(), h, &intents.TnTInquiryBody{Question: "Where is it?"}, intents.TnTInquiryResponse)
	var re *node.RemoteError
	if !errors.As(err, &re) || re.Code != intents.CodeForbidden {
		t.Errorf("the carrier side answered about a customer-side booking: %v", err)
	}

	// One control API, both roles' routes, each reading its own side.
	var sh shipper.ShipmentsResponse
	if code := controlGet(t, cy, "/shipments", &sh); code != http.StatusOK || len(sh.Shipments) != 1 || sh.Shipments[0].Ref != conf.BookingRef {
		t.Errorf("/shipments: HTTP %d %+v", code, sh)
	}
	var bk struct{ Bookings []store.Summary }
	if code := controlGet(t, cy, "/bookings", &bk); code != http.StatusOK || len(bk.Bookings) != 0 {
		t.Errorf("/bookings shows the customer side: HTTP %d %+v", code, bk)
	}
}

// TestReceiverCantBook: a receiver-only node follows others' bookings; it
// can't book, and says so before anything is sent.
func TestReceiverCantBook(t *testing.T) {
	p := startPairWith(t, "", `roles: [receiver]
control: { socket: "`+sockDir(t)+`/cy.sock" }`)
	if _, err := p.shipper.Shipper.Book(context.Background(), "ocean-blue.example", request()); !errors.Is(err, shipper.ErrNoShipperRole) {
		t.Errorf("Book: %v", err)
	}
	b, _ := json.Marshal(shipper.BookRequest{Operator: "ocean-blue.example", Request: *request()})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/book", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	p.shipper.Control.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("/book: HTTP %d %s", rec.Code, rec.Body)
	}
	if p.operator.Carrier.Bookings.Len() != 0 {
		t.Error("the operator got a booking")
	}
	doc, err := p.operator.Keys.Profile(context.Background(), "cymbal-shoes.example")
	if err != nil || !slices.Equal(doc.Profile.Party.Roles, []string{discovery.RoleReceiver}) || !doc.Accepts(intents.TnTEvent) || doc.Accepts(intents.BookingRequest) {
		t.Errorf("receiver profile %+v, %v", doc, err)
	}
}

// TestMergeRefusesSharedIntent: two roles never both handle an intent.
func TestMergeRefusesSharedIntent(t *testing.T) {
	into := map[string]node.Handler{intents.TnTEvent: nil}
	if err := merge(into, map[string]node.Handler{intents.TnTEvent: nil}); err == nil {
		t.Error("merge accepted an intent two roles claim")
	}
	if err := merge(into, map[string]node.Handler{intents.TnTAlert: nil}); err != nil || len(into) != 2 {
		t.Errorf("merge: %v, %v", err, into)
	}
}

// TestBusinessGuardOnlyOnCustomerSide: a carrier that also ships keeps a
// business file, and its guard covers what the customer side sends, not
// the carrier side's replies. Otherwise a partner could book with a
// guessed consignee name and learn from a refused reply that the guess
// is in the private context.
func TestBusinessGuardOnlyOnCustomerSide(t *testing.T) {
	dir := sockDir(t)
	file := filepath.Join(dir, "business.yaml")
	if err := os.WriteFile(file, []byte(`shipments:
  - shipperRef: CY-PO-1
    requiredBy: 2026-10-15
    receivingSlot: { date: 2026-10-15, window: "08:00-12:00", warehouse: "Newark DC dock 4" }
`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := startPairWith(t, "", `roles: [shipper, operator]
business: { file: "`+file+`" }
operator: { refPrefix: CY-BKG, containerPrefix: CYMU, vessel: Sneaker, voyage: 1E, departureHour: 6, transit: 48h }`)

	// Ocean Blue books with Cymbal's carrier side, naming the secret as
	// its consignee: an ordinary confirmation comes back.
	guess := request()
	guess.ShipperName = "Ocean Blue"
	guess.Consignee = &intents.Consignee{Name: "Newark DC dock 4", Domain: "ocean-blue.example"}
	h := intents.HeaderFor(intents.BookingRequest, "ocean-blue.example", "cymbal-shoes.example", time.Now())
	resp, err := p.operator.Node.Exchange(context.Background(), h, guess, intents.BookingConfirmation)
	if err != nil {
		t.Fatalf("the carrier side's reply was refused, which tells the sender the guess is a secret: %v", err)
	}
	if c := resp.Body.(*intents.BookingConfirmationBody); c.Status != intents.StatusConfirmed || c.EBL.ConsigneeName != "Newark DC dock 4" {
		t.Errorf("confirmation %+v", c)
	}

	// Cymbal's own request carrying it is still refused before sending.
	leak := request()
	leak.Consignee = &intents.Consignee{Name: "Newark DC dock 4", Domain: "cymbal-shoes.example"}
	if _, err := p.shipper.Shipper.Book(context.Background(), "ocean-blue.example", leak); !errors.Is(err, business.ErrLeak) {
		t.Errorf("the customer side sent its private context: %v", err)
	}
}

// TestRouteCollision: two roles' control routes colliding fail startup
// with an error, not a panic.
func TestRouteCollision(t *testing.T) {
	route := func(m *http.ServeMux) { m.HandleFunc("GET /bookings", func(http.ResponseWriter, *http.Request) {}) }
	if err := registerRoutes(http.NewServeMux(), route, route); err == nil {
		t.Error("colliding routes were accepted")
	}
	if err := registerRoutes(http.NewServeMux(), route); err != nil {
		t.Errorf("one role's routes: %v", err)
	}
}
