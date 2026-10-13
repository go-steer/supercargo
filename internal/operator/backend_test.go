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
	"fmt"
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
	if err := bookings.Add(withContainer(subscribed(intents.TnTEvent, intents.TnTAlert), "OCBU0000004")); err != nil {
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

// withContainer gives b an eBL with one container.
func withContainer(b store.Booking, container string) store.Booking {
	b.Confirmation.EBL = &intents.EBL{Containers: []intents.ShippingContainer{{ContainerNumber: container}}}
	return b
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
		"another container":    {`{"bookingRef":"OB-BKG-1","intent":"ulp.tnt.event","body":` + strings.Replace(good, `"OCBU0000004"`, `"MSKU1234565"`, 1) + `}`, http.StatusBadRequest},
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
	if conf.BookingRef != "SR-BKG-1042" || conf.SubscriptionID != "SUB-SR-BKG-1042" || s.Bookings.Len() != 1 {
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
	long := *inMemory // a valid 64-character reference: its derived subscription ID would be too long
	long.BookingRef = strings.Repeat("R", 64)
	long.SubscriptionID = ""
	for name, fb := range map[string]*fixedBackend{"an invalid answer": {conf: &bad}, "a failure": {err: errors.New("TMS down")},
		"no answer": {}, "an invalid derived subscription ID": {conf: &long}} {
		s3 := service()
		s3.Backend = fb
		sb := body()
		sb.Subscription = &intents.Subscription{Endpoint: "https://cymbal-shoes.example/ulp", Intents: []string{intents.TnTEvent}}
		if reply, err := s3.Handle(ctx, request("cymbal-shoes.example", sb)); err == nil || s3.Bookings.Len() != 0 {
			t.Errorf("%s: %+v, %v, stored %d", name, reply, err, s3.Bookings.Len())
		}
	}
}

// TestBackendEventRightBooking: while other customers' bookings keep
// arriving, every event lands on the booking it names, never on another
// customer's (a regression: the lookup once took the booking's position
// from one listing and read it from a second, fresh one).
func TestBackendEventRightBooking(t *testing.T) {
	f := &fakePusher{}
	p := publisher(f, &drops{})
	t.Cleanup(p.Close)
	bookings := store.NewBookings()
	named := withContainer(subscribed(), "OCBU0000004") // recorded only: nothing to push
	named.CreatedAt = time.Now().Add(-time.Hour)
	if err := bookings.Add(named); err != nil {
		t.Fatal(err)
	}
	rec := &recordingPublisher{Publishing: p}
	mux := http.NewServeMux()
	RegisterBackend(mux, rec, bookings, p.Log, nil)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { // a partner's booking arriving, newest first, again and again
		defer close(done)
		for i := 2; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = bookings.Add(store.Booking{Ref: fmt.Sprintf("OB-BKG-%d", i), Shipper: "mallory.example", Operator: "ocean-blue.example", CreatedAt: time.Now()})
		}
	}()
	for i := range 300 {
		code, resp := postEvent(t, mux, `{"bookingRef":"OB-BKG-1","intent":"ulp.tnt.event","body":`+eventJSON(t, fmt.Sprintf("EV-%d", i))+`}`)
		if code != http.StatusOK {
			t.Fatalf("event %d: %d %+v", i, code, resp)
		}
	}
	close(stop)
	<-done
	for _, b := range rec.list() {
		if b.Ref != "OB-BKG-1" || b.Shipper != "cymbal-shoes.example" {
			t.Fatalf("an event went to %s of %s, want OB-BKG-1 of cymbal-shoes.example", b.Ref, b.Shipper)
		}
	}
}

// recordingPublisher keeps the bookings it was asked to publish on.
type recordingPublisher struct {
	Publishing
	mu  sync.Mutex
	got []store.Booking
}

func (r *recordingPublisher) Publish(b store.Booking, body intents.Body) error {
	r.mu.Lock()
	r.got = append(r.got, b)
	r.mu.Unlock()
	return r.Publishing.Publish(b, body)
}

func (r *recordingPublisher) list() []store.Booking {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]store.Booking(nil), r.got...)
}

// TestBackendEventNotQueued: recorded but not pushed is a 200 that says
// so: when the booking has no subscription for the intent, and when the
// subscriber's queue is full.
func TestBackendEventNotQueued(t *testing.T) {
	f := &fakePusher{}
	p := publisher(f, &drops{})
	p.QueueLen, p.MaxWorkers = 1, 1
	f.block = make(chan struct{})
	t.Cleanup(func() { close(f.block); p.Close() })
	bookings := store.NewBookings()
	_ = bookings.Add(withContainer(subscribed(intents.TnTEvent), "OCBU0000004")) // alerts aren't subscribed
	var published []string
	mux := http.NewServeMux()
	RegisterBackend(mux, p, bookings, p.Log, func(_ string, body intents.Body) { published = append(published, pushID(body)) })
	if code, resp := postEvent(t, mux, `{"bookingRef":"OB-BKG-1","intent":"ulp.tnt.event","body":`+eventJSON(t, "EV-1")+`}`); code != http.StatusOK || !resp.Queued {
		t.Fatalf("first: %d %+v", code, resp)
	}
	// An alert nobody subscribed to: recorded, not queued.
	if code, resp := postEvent(t, mux, `{"bookingRef":"OB-BKG-1","intent":"ulp.tnt.alert","body":`+strings.Replace(alertTmpl, "%s", `"EV-1"`, 1)+`}`); code != http.StatusOK || resp.Queued {
		t.Errorf("an unsubscribed alert: %d %+v, want 200 not queued", code, resp)
	}
	// Fill the queue (one in flight, one waiting), then one more: recorded,
	// not queued.
	waitFor(t, func() bool { return true })
	_, _ = postEvent(t, mux, `{"bookingRef":"OB-BKG-1","intent":"ulp.tnt.event","body":`+eventJSON(t, "EV-2")+`}`)
	code, resp := postEvent(t, mux, `{"bookingRef":"OB-BKG-1","intent":"ulp.tnt.event","body":`+eventJSON(t, "EV-3")+`}`)
	if code != http.StatusOK || resp.Queued || resp.ID != "EV-3" {
		t.Errorf("a full queue: %d %+v, want 200 not queued", code, resp)
	}
	if got := p.Log.List("ocean-blue.example", "OB-BKG-1"); len(got) != 4 {
		t.Errorf("recorded %d, want 4", len(got))
	}
	if len(published) != 4 {
		t.Errorf("reported %v to the activity stream, want all four", published)
	}
}

// TestBackendAlertOtherBookingsEvent: an alert can't cite an event of
// another booking.
func TestBackendAlertOtherBookingsEvent(t *testing.T) {
	f := &fakePusher{}
	p := publisher(f, &drops{})
	t.Cleanup(p.Close)
	bookings := store.NewBookings()
	_ = bookings.Add(withContainer(subscribed(intents.TnTEvent, intents.TnTAlert), "OCBU0000004"))
	other := withContainer(store.Booking{Ref: "OB-BKG-2", Shipper: "mallory.example", Operator: "ocean-blue.example"}, "OCBU0000004")
	_ = bookings.Add(other)
	mux := http.NewServeMux()
	RegisterBackend(mux, p, bookings, p.Log, nil)
	if code, resp := postEvent(t, mux, `{"bookingRef":"OB-BKG-2","intent":"ulp.tnt.event","body":`+eventJSON(t, "EV-OTHER")+`}`); code != http.StatusOK {
		t.Fatalf("other booking's event: %d %+v", code, resp)
	}
	if code, _ := postEvent(t, mux, `{"bookingRef":"OB-BKG-1","intent":"ulp.tnt.alert","body":`+strings.Replace(alertTmpl, "%s", `"EV-OTHER"`, 1)+`}`); code != http.StatusBadRequest {
		t.Errorf("an alert citing another booking's event: %d, want 400", code)
	}
}
