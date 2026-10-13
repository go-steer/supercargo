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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/backend"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/intents"
)

// backendAPI is the backend API over one subscribed booking, with what it
// published and pushed.
func backendAPI(t *testing.T) (http.Handler, *fakePusher, *Publisher, *[]string) {
	t.Helper()
	f := &fakePusher{}
	p := publisher(f, &drops{})
	t.Cleanup(p.Close)
	bookings := store.NewBookings()
	if err := bookings.Add(subscribed(intents.TnTEvent, intents.TnTAlert)); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var published []string
	mux := http.NewServeMux()
	RegisterBackend(mux, p, bookings, p.Log, func(ref string, body intents.Body) {
		mu.Lock()
		defer mu.Unlock()
		published = append(published, ref+" "+pushID(body))
	})
	return mux, f, p, &published
}

func postEvent(t *testing.T, h http.Handler, body string) (int, EventResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/backend/events", strings.NewReader(body))
	req.Host = "localhost"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var resp EventResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec.Code, resp
}

func eventJSON(t *testing.T, id string) string {
	t.Helper()
	b, err := json.Marshal(ev(id))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const alertTmpl = `{"alert_id":"AL-1","kind":"delay","severity":"high","summary":"Berth congestion at Santos.","cause_event_ids":[%s],` +
	`"previous_eta":"2026-10-24T08:00:00Z","new_eta":"2026-10-27T08:00:00Z"}`

// TestBackendEvent: an event from the carrier's own systems is recorded,
// pushed to the subscriber, and reported to the activity stream.
func TestBackendEvent(t *testing.T) {
	h, f, p, published := backendAPI(t)
	code, resp := postEvent(t, h, `{"bookingRef":"OB-BKG-1","intent":"ulp.tnt.event","body":`+eventJSON(t, "EV-9")+`}`)
	if code != http.StatusOK || resp.ID != "EV-9" || !resp.Queued {
		t.Fatalf("%d %+v", code, resp)
	}
	waitFor(t, func() bool { return len(f.attempts()) == 1 })
	if got := p.Log.List("ocean-blue.example", "OB-BKG-1"); len(got) != 1 || got[0].ID != "EV-9" {
		t.Errorf("log %+v", got)
	}
	if len(*published) != 1 || (*published)[0] != "OB-BKG-1 EV-9" {
		t.Errorf("published %v", *published)
	}
	// The same ID again: a conflict, not a second push.
	if code, _ := postEvent(t, h, `{"bookingRef":"OB-BKG-1","intent":"ulp.tnt.event","body":`+eventJSON(t, "EV-9")+`}`); code != http.StatusConflict {
		t.Errorf("a duplicate: %d, want 409", code)
	}
}

// TestBackendEventRejects: what the carrier's systems send is checked as
// strictly as a partner's message, and nothing is recorded on a refusal.
func TestBackendEventRejects(t *testing.T) {
	h, f, p, published := backendAPI(t)
	good := eventJSON(t, "EV-1")
	for name, c := range map[string]struct {
		body string
		want int
	}{
		"not JSON":             {`{"bookingRef":`, http.StatusBadRequest},
		"an unknown member":    {`{"bookingRef":"OB-BKG-1","intent":"ulp.tnt.event","body":` + good + `,"x":1}`, http.StatusBadRequest},
		"another intent":       {`{"bookingRef":"OB-BKG-1","intent":"ulp.ocean.booking.request","body":{}}`, http.StatusBadRequest},
		"no intent":            {`{"bookingRef":"OB-BKG-1","body":` + good + `}`, http.StatusBadRequest},
		"an unknown booking":   {`{"bookingRef":"OB-BKG-2","intent":"ulp.tnt.event","body":` + good + `}`, http.StatusNotFound},
		"a body member typo":   {`{"bookingRef":"OB-BKG-1","intent":"ulp.tnt.event","body":` + strings.Replace(good, `"event_id"`, `"eventId"`, 1) + `}`, http.StatusBadRequest},
		"an invalid event":     {`{"bookingRef":"OB-BKG-1","intent":"ulp.tnt.event","body":` + strings.Replace(good, `"GTIN"`, `"WARP"`, 1) + `}`, http.StatusBadRequest},
		"an alert citing none": {`{"bookingRef":"OB-BKG-1","intent":"ulp.tnt.alert","body":` + strings.Replace(alertTmpl, "%s", `"EV-404"`, 1) + `}`, http.StatusBadRequest},
		"an oversized body":    {`{"bookingRef":"OB-BKG-1","intent":"ulp.tnt.event","body":"` + strings.Repeat("x", MaxEventBody) + `"}`, http.StatusBadRequest},
	} {
		if code, resp := postEvent(t, h, c.body); code != c.want || resp.Error == "" {
			t.Errorf("%s: %d %+v, want %d with an error", name, code, resp, c.want)
		}
	}
	if got := p.Log.List("ocean-blue.example", "OB-BKG-1"); len(got) != 0 || len(f.attempts()) != 0 || len(*published) != 0 {
		t.Errorf("a refusal recorded or pushed something: log %d, pushes %d, published %v", len(got), len(f.attempts()), *published)
	}
}

// TestBackendAlertCitesHeldEvent: an alert citing an event the booking
// has is accepted.
func TestBackendAlertCitesHeldEvent(t *testing.T) {
	h, f, _, _ := backendAPI(t)
	if code, resp := postEvent(t, h, `{"bookingRef":"OB-BKG-1","intent":"ulp.tnt.event","body":`+eventJSON(t, "EV-5")+`}`); code != http.StatusOK {
		t.Fatalf("event: %d %+v", code, resp)
	}
	code, resp := postEvent(t, h, `{"bookingRef":"OB-BKG-1","intent":"ulp.tnt.alert","body":`+strings.Replace(alertTmpl, "%s", `"EV-5"`, 1)+`}`)
	if code != http.StatusOK || resp.ID != "AL-1" {
		t.Fatalf("alert: %d %+v", code, resp)
	}
	waitFor(t, func() bool { return len(f.attempts()) == 2 })
}

// TestBackendEventPublisherClosed: a closed publisher is a 503, which a
// client may retry later.
func TestBackendEventPublisherClosed(t *testing.T) {
	h, _, p, _ := backendAPI(t)
	p.Close()
	if code, _ := postEvent(t, h, `{"bookingRef":"OB-BKG-1","intent":"ulp.tnt.event","body":`+eventJSON(t, "EV-1")+`}`); code != http.StatusServiceUnavailable {
		t.Errorf("closed: %d, want 503", code)
	}
}

// fixedBackend is a booking system that answers whatever it's told to.
type fixedBackend struct {
	conf *intents.BookingConfirmationBody
	err  error
	got  []backend.BookingRequest
}

func (f *fixedBackend) Decide(_ context.Context, r backend.BookingRequest) (*intents.BookingConfirmationBody, error) {
	f.got = append(f.got, r)
	if f.conf == nil {
		return nil, f.err
	}
	c := *f.conf
	return &c, f.err
}

// TestBookingBackend: the booking service asks its backend, checks the
// answer, stores only a confirmed booking, and never signs an invalid one.
func TestBookingBackend(t *testing.T) {
	ctx := context.Background()
	inMemory, err := (&VoyageBookings{RefPrefix: "SR-BKG", ContainerPrefix: "SRTU", Voyage: Voyage{Vessel: "V", Number: "1W", DepartureHour: 6, Transit: time.Hour}}).
		Decide(ctx, backend.BookingRequest{Shipper: "cymbal-shoes.example", Operator: "ocean-blue.example", Body: body()})
	if err != nil {
		t.Fatal(err)
	}

	fb := &fixedBackend{conf: inMemory}
	s := service()
	s.Backend = fb
	b := body()
	b.Subscription = &intents.Subscription{Endpoint: "https://cymbal-shoes.example/ulp", Intents: []string{intents.TnTEvent}}
	reply, err := s.Handle(ctx, request("cymbal-shoes.example", b))
	if err != nil {
		t.Fatal(err)
	}
	conf := reply.Body.(*intents.BookingConfirmationBody)
	if conf.BookingRef != "SR-BKG-1042" || conf.SubscriptionID == "" || s.Bookings.Len() != 1 {
		t.Errorf("confirmed: %+v, stored %d", conf, s.Bookings.Len())
	}
	if len(fb.got) != 1 || fb.got[0].Shipper != "cymbal-shoes.example" || fb.got[0].Operator != "ocean-blue.example" {
		t.Errorf("the backend was asked %+v", fb.got)
	}

	// A rejection goes back as it is, and nothing is stored.
	s2 := service()
	s2.Backend = &fixedBackend{conf: &intents.BookingConfirmationBody{BookingRef: "X-1", Status: intents.StatusRejected,
		Messages: []intents.Message{{Code: "no_capacity", Detail: "Full."}}}}
	if reply, err := s2.Handle(ctx, request("cymbal-shoes.example", body())); err != nil ||
		reply.Body.(*intents.BookingConfirmationBody).Status != intents.StatusRejected || s2.Bookings.Len() != 0 {
		t.Errorf("a rejection: %+v, %v, stored %d", reply, err, s2.Bookings.Len())
	}

	// An invalid answer is never signed, and a backend failure is an error.
	bad := *inMemory
	bad.PlannedArrival = "soon"
	for name, fb := range map[string]*fixedBackend{"an invalid answer": {conf: &bad}, "a failure": {err: errors.New("TMS down")}} {
		s3 := service()
		s3.Backend = fb
		if reply, err := s3.Handle(ctx, request("cymbal-shoes.example", body())); err == nil || s3.Bookings.Len() != 0 {
			t.Errorf("%s: %+v, %v, stored %d", name, reply, err, s3.Bookings.Len())
		}
	}
}
