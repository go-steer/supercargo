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
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/control"
	"github.com/go-steer/supercargo/internal/llm"
	"github.com/go-steer/supercargo/internal/llm/fake"
	"github.com/go-steer/supercargo/internal/operator"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/intents"
)

// carrierNode is an in-memory CarrierNode: three bookings on 42W (one
// arrived), one on 43E.
type carrierNode struct {
	mu        sync.Mutex
	disrupted []operator.DisruptRequest
	fail      error // Disrupt fails with it, if set
}

var carrierBookings = []store.Summary{
	{Ref: "OB-BKG-1045", Shipper: "cymbal-shoes.example", Vessel: "Ocean Blue Aurora", Voyage: "42W", Status: "in transit", ETA: "2026-10-13T08:00:00Z",
		Origin: intents.Location{UNLocode: "NLRTM"}, Destination: intents.Location{UNLocode: "USEWR"}},
	{Ref: "OB-BKG-1044", Shipper: "north-shoes.example", Vessel: "Ocean Blue Aurora", Voyage: "42W", Status: "in transit", ETA: "2026-10-13T08:00:00Z", Alerts: 1,
		Origin: intents.Location{UNLocode: "NLRTM"}, Destination: intents.Location{UNLocode: "USEWR"}},
	{Ref: "OB-BKG-1043", Shipper: "cymbal-shoes.example", Vessel: "Ocean Blue Aurora", Voyage: "42W", Status: "arrived", ETA: "2026-10-06T08:00:00Z",
		Origin: intents.Location{UNLocode: "NLRTM"}, Destination: intents.Location{UNLocode: "USEWR"}},
	{Ref: "OB-BKG-1042", Shipper: "west-shoes.example", Vessel: "Ocean Blue Breeze", Voyage: "43E", Status: "loaded", ETA: "2026-10-20T08:00:00Z",
		Origin: intents.Location{UNLocode: "USEWR"}, Destination: intents.Location{UNLocode: "NLRTM"}},
}

func (n *carrierNode) Bookings(context.Context) ([]store.Summary, error) { return carrierBookings, nil }

func (n *carrierNode) BookingRecords(_ context.Context, ref string) (*operator.BookingRecordsResponse, error) {
	for _, s := range carrierBookings {
		if s.Ref == ref {
			var recs []store.RecordView
			for range 40 {
				recs = append(recs, store.RecordView{ID: "EV-x", Intent: intents.TnTEvent, Body: []byte(`{}`)})
			}
			return &operator.BookingRecordsResponse{Summary: &s, Records: recs}, nil
		}
	}
	return nil, &NodeError{Status: http.StatusNotFound, Message: "no booking " + ref}
}

func (n *carrierNode) Disrupt(_ context.Context, req operator.DisruptRequest) (*operator.DisruptResponse, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.fail != nil {
		return nil, n.fail
	}
	n.disrupted = append(n.disrupted, req)
	alert := func(ref string) *intents.TnTAlertBody {
		return &intents.TnTAlertBody{AlertID: "AL-" + ref[len(ref)-4:], Kind: intents.AlertDelay, Severity: intents.SeverityHigh, Summary: req.Reason,
			PreviousETA: "2026-10-13T08:00:00Z", NewETA: "2026-10-16T08:00:00Z", CauseEventIDs: []string{"EV-1"}}
	}
	if req.Voyage != "" {
		return &operator.DisruptResponse{Alerts: []operator.VoyageAlert{{BookingRef: "OB-BKG-1044", Alert: alert("OB-BKG-1044")},
			{BookingRef: "OB-BKG-1045", Applied: true, Error: "alert not sent"}}}, nil
	}
	return &operator.DisruptResponse{Alert: alert(req.BookingRef)}, nil
}

func carrierAssistant(m llm.Model, n *carrierNode) (*Assistant, *[]ToolEvent) {
	var tools []ToolEvent
	a := &Assistant{Model: m, Carrier: n, Drafts: &Drafts{}, Now: func() time.Time { return thursday },
		Self:   Self{Domain: "ocean-blue.example", Name: "Ocean Blue"},
		OnTool: func(e ToolEvent) { tools = append(tools, e) }}
	return a, &tools
}

func carrierCall(t *testing.T, a *Assistant, tools *[]ToolEvent, call llm.ToolCall) (ToolEvent, string) {
	t.Helper()
	a.Model = fake.Turns(llm.GenerateResponse{ToolCalls: []llm.ToolCall{call}}, llm.GenerateResponse{Text: "OK."})
	*tools = nil
	if _, err := a.Turn(context.Background(), &Conversation{}, "go"); err != nil {
		t.Fatal(err)
	}
	if len(*tools) != 1 {
		t.Fatalf("tools %+v", *tools)
	}
	return (*tools)[0], lastResult(a)
}

func refsIn(t *testing.T, res string) string {
	t.Helper()
	var r struct{ Bookings []store.Summary }
	_ = json.Unmarshal([]byte(res), &r)
	var refs []string
	for _, s := range r.Bookings {
		refs = append(refs, s.Ref)
	}
	return strings.Join(refs, ",")
}

func TestCarrierListBookings(t *testing.T) {
	a, tools := carrierAssistant(nil, &carrierNode{})
	for in, want := range map[string]string{
		`{}`:                               "OB-BKG-1045,OB-BKG-1044,OB-BKG-1043,OB-BKG-1042",
		`{"voyage":"42w"}`:                 "OB-BKG-1045,OB-BKG-1044,OB-BKG-1043",
		`{"vessel":"ocean blue breeze"}`:   "OB-BKG-1042",
		`{"port":"USEWR","voyage":"43E"}`:  "OB-BKG-1042",
		`{"arrivingBefore":"2026-10-10"}`:  "OB-BKG-1043",
		`{"delayed":true}`:                 "OB-BKG-1044",
		`{"voyage":"42W","delayed":false}`: "OB-BKG-1045,OB-BKG-1044,OB-BKG-1043",
	} {
		ev, res := carrierCall(t, a, tools, fake.Call("1", ToolListBookings, in))
		if ev.Error != "" || refsIn(t, res) != want {
			t.Errorf("%s: %q, %s", in, refsIn(t, res), ev.Error)
		}
	}
	for _, bad := range []string{`{"port":"Rotterdam"}`, `{"port":"nlrtm"}`, `{"arrivingBefore":"next week"}`, `{"customer":"x"}`} {
		if ev, _ := carrierCall(t, a, tools, fake.Call("1", ToolListBookings, bad)); ev.Error == "" {
			t.Errorf("%s: accepted", bad)
		}
	}
}

func TestCarrierGetBooking(t *testing.T) {
	a, tools := carrierAssistant(nil, &carrierNode{})
	ev, res := carrierCall(t, a, tools, fake.Call("1", ToolGetBooking, `{"bookingRef":"OB-BKG-1045"}`))
	var r operator.BookingRecordsResponse
	if ev.Error != "" || json.Unmarshal([]byte(res), &r) != nil || r.Summary.Ref != "OB-BKG-1045" || len(r.Records) != maxRecords {
		t.Errorf("get_booking: %v, %d records", ev.Error, len(r.Records))
	}
	if ev, _ := carrierCall(t, a, tools, fake.Call("1", ToolGetBooking, `{"bookingRef":"OB-BKG-9"}`)); ev.Error == "" {
		t.Error("an unknown booking was found")
	}
}

// TestCarrierDraftDisruption: a delay is drafted for one booking or the
// voyage's bookings under way, never published by the tool; every bad
// shape is refused.
func TestCarrierDraftDisruption(t *testing.T) {
	n := &carrierNode{}
	a, tools := carrierAssistant(nil, n)
	ev, _ := carrierCall(t, a, tools, fake.Call("1", ToolDraftDisruption, `{"voyage":"42W","delayHours":72,"reason":"Engine repair,   unscheduled call at Halifax."}`))
	d := ev.Draft
	if ev.Error != "" || d == nil || d.Disruption == nil || d.DraftID != "DD-0001" || d.Request != nil ||
		strings.Join(d.Disruption.Bookings, ",") != "OB-BKG-1044,OB-BKG-1045" || d.Disruption.Delay != "72h0m0s" ||
		d.Disruption.Reason != "Engine repair, unscheduled call at Halifax." {
		t.Fatalf("voyage draft: %+v %v", d, ev.Error)
	}
	if len(n.disrupted) != 0 {
		t.Fatal("a draft published a delay")
	}
	if ev, _ := carrierCall(t, a, tools, fake.Call("1", ToolDraftDisruption, `{"bookingRef":"OB-BKG-1042","delayHours":12,"reason":"Crane failure."}`)); ev.Draft == nil ||
		strings.Join(ev.Draft.Disruption.Bookings, ",") != "OB-BKG-1042" {
		t.Errorf("booking draft: %+v", ev)
	}
	for name, in := range map[string]string{
		"both":            `{"bookingRef":"OB-BKG-1042","voyage":"42W","delayHours":1,"reason":"x."}`,
		"neither":         `{"delayHours":1,"reason":"x."}`,
		"vessel alone":    `{"bookingRef":"OB-BKG-1042","vessel":"V","delayHours":1,"reason":"x."}`,
		"no reason":       `{"bookingRef":"OB-BKG-1042","delayHours":1,"reason":"  "}`,
		"hidden reason":   `{"bookingRef":"OB-BKG-1042","delayHours":1,"reason":"Late\u202e."}`,
		"unknown booking": `{"bookingRef":"OB-BKG-9","delayHours":1,"reason":"x."}`,
		"nothing on it":   `{"voyage":"99X","delayHours":1,"reason":"x."}`,
		"too long":        `{"bookingRef":"OB-BKG-1042","delayHours":721,"reason":"x."}`,
		"no delay":        `{"bookingRef":"OB-BKG-1042","delayHours":0,"reason":"x."}`,
	} {
		if ev, _ := carrierCall(t, a, tools, fake.Call("1", ToolDraftDisruption, in)); ev.Error == "" || ev.Draft != nil {
			t.Errorf("%s: accepted %+v", name, ev)
		}
	}
	// A carrier assistant has no shipper tools, and the reverse.
	if ev, _ := carrierCall(t, a, tools, fake.Call("1", ToolDraftBooking, draftJSON(""))); ev.Error == "" {
		t.Error("the carrier's assistant drafted a booking")
	}
	s, stools, _ := newAssistant(nil, &node{})
	s.Model = fake.Turns(llm.GenerateResponse{ToolCalls: []llm.ToolCall{fake.Call("1", ToolDraftDisruption, `{"bookingRef":"OB-BKG-1042","delayHours":1,"reason":"x."}`)}},
		llm.GenerateResponse{Text: "OK."})
	_, _ = s.Turn(context.Background(), &Conversation{}, "delay it")
	if len(*stools) != 1 || (*stools)[0].Error == "" {
		t.Errorf("the shipper's assistant drafted a delay: %+v", *stools)
	}
}

// TestCarrierSend: a confirmed delay is published once, through the
// node; refused or unreachable, it stays a draft; anything else is gone.
func TestCarrierSend(t *testing.T) {
	n := &carrierNode{}
	a, tools := carrierAssistant(nil, n)
	ev, _ := carrierCall(t, a, tools, fake.Call("1", ToolDraftDisruption, `{"voyage":"42W","vessel":"Ocean Blue Aurora","delayHours":72,"reason":"Engine repair."}`))
	id := ev.Draft.DraftID
	sent, err := a.Send(context.Background(), id)
	if err != nil || sent.Disrupted == nil || len(sent.Disrupted.Alerts) != 2 || len(n.disrupted) != 1 ||
		n.disrupted[0] != (operator.DisruptRequest{Vessel: "Ocean Blue Aurora", Voyage: "42W", Delay: "72h0m0s", Reason: "Engine repair."}) {
		t.Fatalf("send: %+v, %v, %+v", sent, err, n.disrupted)
	}
	if _, err := a.Send(context.Background(), id); !errors.Is(err, ErrNoDraft) {
		t.Errorf("a second send: %v", err)
	}
	// The report names the booking that was delayed without its alert.
	a.Model = FakeCarrierModel()
	r, err := a.Report(context.Background(), &Conversation{}, sent)
	if err != nil || !strings.Contains(r.Text, "OB-BKG-1044 (AL-1044)") || !strings.Contains(r.Text, "don't publish again") {
		t.Errorf("report: %q, %v", r.Text, err)
	}
	for name, c := range map[string]struct {
		err  error
		kept bool
	}{
		"refused":     {&NodeError{Status: http.StatusConflict, Message: "arrived"}, true},
		"not running": {&net.OpError{Op: "dial", Err: errors.New("no socket")}, true},
		"unknown":     {errors.New("EOF"), false},
	} {
		n.fail = c.err
		ev, _ := carrierCall(t, a, tools, fake.Call("1", ToolDraftDisruption, `{"bookingRef":"OB-BKG-1045","delayHours":1,"reason":"x."}`))
		_, err := a.Send(context.Background(), ev.Draft.DraftID)
		_, still := a.Drafts.Get(ev.Draft.DraftID)
		if err == nil || (still == nil) != c.kept || (!c.kept && !errors.Is(err, ErrOutcomeUnknown)) {
			t.Errorf("%s: %v, kept %v", name, err, still == nil)
		}
	}
	// A shipper's assistant can't publish a delay, even with a draft.
	s, _, _ := newAssistant(nil, &node{})
	dr, _ := s.Drafts.add(Draft{Disruption: &Disruption{BookingRef: "OB-BKG-1", Delay: "1h0m0s", Reason: "x."}})
	if _, err := s.Send(context.Background(), dr.ID); err == nil {
		t.Error("a shipper's assistant published a delay")
	}
}

// TestControlCarrierNode: the carrier's node over its control API.
func TestControlCarrierNode(t *testing.T) {
	var got operator.DisruptRequest
	mux := http.NewServeMux()
	mux.HandleFunc("GET /bookings", func(w http.ResponseWriter, _ *http.Request) {
		control.Answer(w, http.StatusOK, operator.BookingsResponse{Bookings: carrierBookings})
	})
	mux.HandleFunc("GET /bookings/{ref}/events", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("ref") != "OB-BKG-1045" {
			control.Answer(w, http.StatusNotFound, operator.BookingRecordsResponse{Error: "no booking"})
			return
		}
		control.Answer(w, http.StatusOK, operator.BookingRecordsResponse{Summary: &carrierBookings[0]})
	})
	mux.HandleFunc("POST /disrupt", func(w http.ResponseWriter, r *http.Request) {
		_ = control.Decode(r, &got)
		control.Answer(w, http.StatusOK, operator.DisruptResponse{Alerts: []operator.VoyageAlert{{BookingRef: "OB-BKG-1045"}}})
	})
	n := ControlCarrierNode{Client: control.InProcess(mux, time.Second)}
	ctx := context.Background()
	if bs, err := n.Bookings(ctx); err != nil || len(bs) != 4 {
		t.Errorf("bookings: %d, %v", len(bs), err)
	}
	if r, err := n.BookingRecords(ctx, "OB-BKG-1045"); err != nil || r.Summary.Ref != "OB-BKG-1045" {
		t.Errorf("records: %+v, %v", r, err)
	}
	var ne *NodeError
	if _, err := n.BookingRecords(ctx, "OB/../x"); !errors.As(err, &ne) || ne.Status != http.StatusNotFound {
		t.Errorf("an unknown booking: %v", err)
	}
	if r, err := n.Disrupt(ctx, operator.DisruptRequest{Voyage: "42W", Delay: "1h0m0s", Reason: "x."}); err != nil || len(r.Alerts) != 1 || got.Voyage != "42W" {
		t.Errorf("disrupt: %+v, %v, sent %+v", r, err, got)
	}
}

// TestFakeCarrierScenes: the carrier scenes with the fake model.
func TestFakeCarrierScenes(t *testing.T) {
	n := &carrierNode{}
	a, _ := carrierAssistant(FakeCarrierModel(), n)
	var c Conversation
	turn := func(text string) Reply {
		t.Helper()
		r, err := a.Turn(context.Background(), &c, text)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := turn("Which bookings are on voyage 42W?"); !strings.Contains(r.Text, "3 booking(s)") || !strings.Contains(r.Text, "**OB-BKG-1044** for `north-shoes.example`") {
		t.Errorf("list: %q", r.Text)
	}
	if r := turn("What's happening with OB-BKG-1044, is it late?"); !strings.Contains(r.Text, "**OB-BKG-1044**") || len(r.Drafts) != 0 {
		t.Errorf("story: %q", r.Text)
	}
	r := turn("Engine trouble on the Aurora. Delay voyage 42W by 3 days: engine repair, unscheduled call at Halifax")
	if len(r.Drafts) != 1 || r.Drafts[0].Disruption == nil || r.Drafts[0].Disruption.Delay != "72h0m0s" ||
		r.Drafts[0].Disruption.Reason != "Engine repair, unscheduled call at Halifax." || !strings.Contains(r.Text, "Nothing is published yet") {
		t.Fatalf("delay: %+v", r)
	}
	if r := turn("Delay OB-BKG-1042 by 12 hours"); len(r.Drafts) != 1 || r.Drafts[0].Disruption.Delay != "12h0m0s" || r.Drafts[0].Disruption.Reason != "Operational delay." {
		t.Errorf("a booking's delay: %+v", r)
	}
	if r := turn("hello"); !strings.Contains(r.Text, "prepare a delay") {
		t.Errorf("hello: %q", r.Text)
	}
	if r := turn("Delay voyage 99X by 1 day: fog"); !strings.Contains(r.Text, "didn't work") {
		t.Errorf("nothing on the voyage: %q", r.Text)
	}
	if len(n.disrupted) != 0 {
		t.Error("a scene published a delay")
	}
}
