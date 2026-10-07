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

package shipper

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/business"
	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/discovery"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
	"github.com/go-steer/supercargo/pkg/sig"
)

type profiles struct {
	doc *discovery.Document
	err error
}

func (p profiles) Profile(context.Context, string) (*discovery.Document, error) { return p.doc, p.err }

func oceanBlueProfile(t *testing.T) *discovery.Document {
	t.Helper()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	j, _ := sig.Ed25519JWK(pub, "ob-1")
	b, _ := json.Marshal(discovery.Profile{
		ProtocolVersion: envelope.Version,
		Party:           discovery.Party{Domain: "ocean-blue.example", Name: "Ocean Blue", Roles: []string{"operator"}},
		Services:        []discovery.Service{{Name: "ulp.freight", Intents: []string{"ulp.booking.request", "ulp.tnt.inquiry"}, Extensions: []string{}}},
		Transports:      discovery.Transports{REST: "https://ocean-blue.example/ulp", A2A: "https://ocean-blue.example/a2a"},
		Keys:            []json.RawMessage{j},
	})
	doc, err := discovery.ParseProfile(b, discovery.ParseOptions{Domain: "ocean-blue.example"})
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func get(t *testing.T, h http.Handler, path string, v any) int {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("%s: %v (%s)", path, err, rec.Body)
	}
	return rec.Code
}

// TestControlLeakRefusal: a send the business-context guard refuses is a
// 422 that says why, not a 502 blaming the partner.
func TestControlLeakRefusal(t *testing.T) {
	biz, err := business.Parse([]byte(`notes: ["Spring launch stock: shelves full by the 16th"]`))
	if err != nil {
		t.Fatal(err)
	}
	s := shipperWith(t, func(envelope.Header) (string, any) { return intents.TnTInquiryResponse, nil },
		func(c *node.Config) { c.OutboundGuard = biz.Guard() })
	_ = s.Bookings.Add(store.Booking{Ref: "OB-BKG-1", Operator: "ocean-blue.example", Shipper: "cymbal-shoes.example"})
	h := ControlHandler(s, profiles{})
	b, _ := json.Marshal(AskRequest{BookingRef: "OB-BKG-1", Question: "Spring launch stock: shelves full by the 16th. Possible?"})
	req := httptest.NewRequest(http.MethodPost, "/ask", strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var resp AskResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(resp.Error, "private business context") {
		t.Errorf("a leaking question: HTTP %d %q", rec.Code, resp.Error)
	}
	bb, _ := json.Marshal(BookRequest{Operator: "ocean-blue.example", Request: func() intents.BookingRequestBody {
		r := *request()
		r.Cargo.Description = "Spring launch stock: shelves full by the 16th"
		return r
	}()})
	req = httptest.NewRequest(http.MethodPost, "/book", strings.NewReader(string(bb)))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("a leaking booking: HTTP %d %s", rec.Code, rec.Body)
	}
}

func TestControlReads(t *testing.T) {
	s := answering(t, "x", nil, "")
	ev, _ := json.Marshal(intents.TnTEventBody{EventID: "EV-2", EventType: "TRANSPORT", EventTypeCode: "DEPA", EventClassifierCode: "ACT",
		EventDateTime: "2026-10-07T18:00:00Z"})
	if _, err := s.Events.Add("ocean-blue.example", "OB-BKG-1", store.Record{ID: "EV-2", Intent: intents.TnTEvent, Body: ev,
		At: time.Now(), Header: envelope.Header{Sender: "ocean-blue.example", MessageID: "m-2"}}); err != nil {
		t.Fatal(err)
	}
	h := ControlHandler(s, profiles{doc: oceanBlueProfile(t)})

	var sh ShipmentsResponse
	if code := get(t, h, "/shipments", &sh); code != http.StatusOK || len(sh.Shipments) != 1 ||
		sh.Shipments[0].Ref != "OB-BKG-1" || sh.Shipments[0].Status != "in transit" || sh.Shipments[0].Events != 1 {
		t.Errorf("shipments: HTTP %d %+v", code, sh)
	}
	var rr RecordsResponse
	if code := get(t, h, "/shipments/ocean-blue.example/OB-BKG-1/events", &rr); code != http.StatusOK || len(rr.Records) != 2 ||
		rr.Records[1].ID != "EV-2" || rr.Records[1].MessageID != "m-2" || rr.Summary == nil {
		t.Errorf("records: HTTP %d %+v", code, rr)
	}
	if code := get(t, h, "/shipments/other.example/OB-BKG-1/events", &rr); code != http.StatusNotFound || rr.Error == "" {
		t.Errorf("another operator's ref: HTTP %d %+v", code, rr)
	}
	var pr ProfileResponse
	if code := get(t, h, "/profile/ocean-blue.example", &pr); code != http.StatusOK || pr.Profile == nil ||
		pr.Profile.Name != "Ocean Blue" || pr.Profile.A2A == "" || len(pr.Profile.Keys) != 1 || pr.Profile.Keys[0] != "ob-1" ||
		len(pr.Profile.Intents) != 2 {
		t.Errorf("profile: HTTP %d %+v", code, pr.Profile)
	}
	if code := get(t, h, "/profile/Not%20A%20Domain", &pr); code != http.StatusBadRequest {
		t.Errorf("bad domain: HTTP %d", code)
	}
	failing := ControlHandler(s, profiles{err: errors.New("no such host")})
	if code := get(t, failing, "/profile/mallory.example", &pr); code != http.StatusBadGateway || pr.Error == "" {
		t.Errorf("fetch failure: HTTP %d %+v", code, pr)
	}
	// Without an event log, shipments still list.
	s.Events = nil
	if code := get(t, h, "/shipments", &sh); code != http.StatusOK || len(sh.Shipments) != 1 || sh.Shipments[0].Events != 0 {
		t.Errorf("no event log: HTTP %d %+v", code, sh)
	}
}
