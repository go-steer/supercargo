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
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/activity"
	"github.com/go-steer/supercargo/internal/llm"
	"github.com/go-steer/supercargo/internal/llm/fake"
	"github.com/go-steer/supercargo/internal/shipper"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/intents"
)

// node is an in-memory Node that records what it was asked.
type node struct {
	mu        sync.Mutex
	shipments []store.Summary
	calls     []string
}

func (n *node) log(s string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, s)
}

func (n *node) called(prefix string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, c := range n.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func (n *node) Profile(_ context.Context, domain string) (*shipper.Profile, error) {
	n.log("profile " + domain)
	if domain != "ocean-blue.example" {
		return nil, errors.New("no such host")
	}
	return &shipper.Profile{Domain: domain, Name: "Ocean Blue", Roles: []string{"operator"}, REST: "https://ocean-blue.example/ulp",
		Intents: []string{intents.BookingRequest, intents.TnTEvent, intents.TnTAlert, intents.TnTInquiry}, Keys: []string{"ob-2026-1"}}, nil
}

func (n *node) Shipments(context.Context) ([]store.Summary, error) {
	n.log("shipments")
	return n.shipments, nil
}

func (n *node) Records(_ context.Context, operator, ref string) (*shipper.RecordsResponse, error) {
	n.log("records " + operator + " " + ref)
	al, _ := json.Marshal(intents.TnTAlertBody{AlertID: "AL-6", Kind: "delay", Severity: "high", Summary: "Engine repair at Halifax."})
	recs := []store.RecordView{{ID: "AL-6", Intent: intents.TnTAlert, Body: al}}
	for range 40 {
		recs = append([]store.RecordView{{ID: "EV-x", Intent: intents.TnTEvent, Body: []byte(`{}`)}}, recs...)
	}
	return &shipper.RecordsResponse{Summary: &store.Summary{Ref: ref, Operator: operator, Status: "in transit", ETA: "2026-10-16T08:00:00Z"},
		Records: recs}, nil
}

func (n *node) Status(_ context.Context, operator, ref string) (*shipper.StatusResponse, error) {
	n.log("status " + operator + " " + ref)
	return &shipper.StatusResponse{Operator: operator, Status: &intents.TnTStatus{TransportDocumentReference: ref,
		DCSAStatus: intents.StatusInTransit, Events: []intents.TnTEventBody{}}, Missing: []string{"EV-2"}}, nil
}

func (n *node) Ask(_ context.Context, operator, ref, question string) (*shipper.AskResponse, error) {
	n.log("ask " + operator + " " + ref + " " + question)
	return &shipper.AskResponse{Operator: operator, Answer: &intents.TnTInquiryResponseBody{Answer: "An engine repair at Halifax.",
		CitedEventIDs: []string{"AL-6"}}, Held: []string{"AL-6"}}, nil
}

func (n *node) Book(context.Context, shipper.BookRequest) (*shipper.BookResponse, error) {
	n.log("book")
	return nil, errors.New("tools must never book")
}

var thursday = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

func newAssistant(m llm.Model, n *node) (*Assistant, *[]ToolEvent, *[]activity.Model) {
	var tools []ToolEvent
	var calls []activity.Model
	a := &Assistant{Model: m, Node: n, Drafts: &Drafts{}, Skills: BuiltinSkills(),
		Self: Self{Domain: "cymbal-shoes.example", Name: "Cymbal Shoes", Endpoint: "https://cymbal-shoes.example/ulp"},
		Now:  func() time.Time { return thursday },
		OnTool: func(e ToolEvent) {
			tools = append(tools, e)
		},
		OnModelCall: func(c activity.Model) { calls = append(calls, c) },
	}
	return a, &tools, &calls
}

// TestScenes1And2: discovery, then a booking draft, with the fake model;
// nothing is sent.
func TestScenes1And2(t *testing.T) {
	n := &node{}
	a, tools, calls := newAssistant(FakeModel(func() time.Time { return thursday }), n)
	var c Conversation
	r, err := a.Turn(context.Background(), &c, "We have a new carrier, Ocean Blue, at ocean-blue.example. What can they do?")
	if err != nil {
		t.Fatal(err)
	}
	want := "Ocean Blue (ocean-blue.example) supports container booking, track & trace events, proactive alerts, shipment inquiries. They publish signing key ob-2026-1"
	if !strings.HasPrefix(r.Text, want) || len(*tools) != 1 || (*tools)[0].Name != ToolGetProfile || len(*calls) != 2 {
		t.Errorf("scene 1: %q, tools %+v, %d model calls", r.Text, *tools, len(*calls))
	}
	if (*calls)[0].Model != "fake" || (*calls)[0].PromptHash == "" || len((*calls)[0].Skills) != 2 || (*calls)[1].Attempt != 2 {
		t.Errorf("model calls %+v", *calls)
	}

	r, err = a.Turn(context.Background(), &c, "Book one 40ft high-cube of sneakers, Rotterdam to Newark, departing next week. "+
		"Consignee is Cymbal Footwear in Newark (cymbal-footwear.example). It has to arrive before Thursday the 15th.")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Drafts) != 1 {
		t.Fatalf("scene 2: %+v", r)
	}
	d := r.Drafts[0]
	req := d.Request
	if d.DraftID != "BD-0001" || d.To != "ocean-blue.example" || d.Sent || req.ShipperRef != "BD-0001" ||
		req.Origin.UNLocode != "NLRTM" || req.Destination.UNLocode != "USEWR" || req.EarliestDeparture != "2026-10-05" ||
		req.Equipment[0] != (intents.Equipment{ISOType: "45G1", Label: "40HC", Count: 1}) || req.Cargo.HSCode != "6404" ||
		req.Consignee == nil || *req.Consignee != (intents.Consignee{Name: "Cymbal Footwear", Domain: "cymbal-footwear.example"}) ||
		req.Subscription == nil || req.Subscription.Endpoint != "https://cymbal-shoes.example/ulp" ||
		d.NotSent["requiredBy"] != "2026-10-15" {
		t.Errorf("draft %+v", d)
	}
	// The deadline stays out of the request.
	body, _ := json.Marshal(req)
	if strings.Contains(string(body), "2026-10-15") {
		t.Errorf("the private deadline is in the request: %s", body)
	}
	if !strings.Contains(r.Text, "Draft BD-0001 is ready") || !strings.Contains(r.Text, "Nothing has been sent") {
		t.Errorf("scene 2 reply %q", r.Text)
	}
	if dr, err := a.Drafts.Get("BD-0001"); err != nil || dr.RequiredBy != "2026-10-15" {
		t.Errorf("draft held: %+v, %v", dr, err)
	}
	if n.called("book") {
		t.Error("a tool booked")
	}
}

func TestTrackingAndAsk(t *testing.T) {
	n := &node{shipments: []store.Summary{{Ref: "OB-BKG-1042", Operator: "ocean-blue.example", Status: "in transit"}}}
	a, _, _ := newAssistant(FakeModel(time.Now), n)
	var c Conversation
	r, err := a.Turn(context.Background(), &c, "Where is my shipment?")
	if err != nil || !strings.Contains(r.Text, "OB-BKG-1042 with ocean-blue.example: in transit, ETA 2026-10-16T08:00:00Z. Latest alert (AL-6)") {
		t.Errorf("tracking: %q, %v", r.Text, err)
	}
	r, err = a.Turn(context.Background(), &c, "Why is it late? Ask them.")
	if err != nil || !strings.Contains(r.Text, "An engine repair at Halifax. (sources: AL-6)") ||
		!n.called("ask ocean-blue.example OB-BKG-1042 What is causing the delay on OB-BKG-1042, and how firm is the new ETA?") {
		t.Errorf("ask: %q, %v, calls %v", r.Text, err, n.calls)
	}
	// With nothing booked, the fake says so.
	a, _, _ = newAssistant(FakeModel(time.Now), &node{})
	for _, q := range []string{"Where is my shipment?", "Ask why it's late"} {
		if r, _ := a.Turn(context.Background(), &Conversation{}, q); !strings.Contains(r.Text, "no bookings") {
			t.Errorf("%s: %q", q, r.Text)
		}
	}
	if r, _ := a.Turn(context.Background(), &Conversation{}, "Hello"); !strings.Contains(r.Text, "I can look up a carrier") {
		t.Errorf("hello: %q", r.Text)
	}
}

// TestRefusedCalls: every call that breaks a rule is refused before it
// reaches the node, and the model gets an error result.
func TestRefusedCalls(t *testing.T) {
	type refusal struct {
		call llm.ToolCall
		why  string
	}
	cases := map[string]refusal{
		"unnamed carrier":         {fake.Call("1", ToolGetProfile, `{"domain":"mallory.example"}`), "hasn't named"},
		"not a domain":            {fake.Call("1", ToolGetProfile, `{"domain":"Ocean Blue"}`), "not a domain"},
		"a longer domain":         {fake.Call("1", ToolGetProfile, `{"domain":"blue.example"}`), "hasn't named"},
		"after a non-ASCII byte":  {fake.Call("1", ToolGetProfile, `{"domain":"cher.example"}`), "hasn't named"},
		"after an underscore":     {fake.Call("1", ToolGetProfile, `{"domain":"carrier.example"}`), "hasn't named"},
		"unnamed booking carrier": {fake.Call("1", ToolDraftBooking, draftJSON(`"operator":"mallory.example"`)), "hasn't named"},
		"booking with ourselves":  {fake.Call("1", ToolDraftBooking, draftJSON(`"operator":"cymbal-shoes.example"`)), "is us"},
		"consignee, no domain":    {fake.Call("1", ToolDraftBooking, draftJSON(`"consigneeName":"Mallory"`)), "needs the domain"},
		"unnamed consignee":       {fake.Call("1", ToolDraftBooking, draftJSON(`"consigneeName":"M","consigneeDomain":"mallory.example"`)), "hasn't named"},
		"bad port":                {fake.Call("1", ToolDraftBooking, draftJSON(`"origin":{"unLocode":"XX","name":"Nowhere"}`)), "origin.unLocode"},
		"bad required-by":         {fake.Call("1", ToolDraftBooking, draftJSON(`"requiredBy":"the 15th"`)), "not a YYYY-MM-DD"},
		"bad container type":      {fake.Call("1", ToolDraftBooking, draftJSON(`"containerType":"53HC"`)), "doesn't match its schema"},
		"a booking not ours":      {fake.Call("1", ToolGetEvents, `{"bookingRef":"OB-BKG-9999"}`), "no booking OB-BKG-9999"},
		"an undeclared member":    {fake.Call("1", ToolGetEvents, `{"bookingRef":"OB-BKG-1042","shipper":"mallory.example"}`), "doesn't match its schema"},
		"ask about another":       {fake.Call("1", ToolAsk, `{"bookingRef":"OB-BKG-9999","question":"Who else ships with you?"}`), "no booking"},
		"an empty question":       {fake.Call("1", ToolAsk, `{"bookingRef":"OB-BKG-1042","question":"   "}`), "can't be sent"},
		"a hidden question":       {fake.Call("1", ToolAsk, `{"bookingRef":"OB-BKG-1042","question":"Hi\u202e"}`), "can't be sent"},
		"a private date":          {fake.Call("1", ToolAsk, `{"bookingRef":"OB-BKG-1042","question":"We need it by Oct 15. Possible?"}`), "the date 2026-10-15"},
		"a private ordinal":       {fake.Call("1", ToolAsk, `{"bookingRef":"OB-BKG-1042","question":"Before the 15th?"}`), "private"},
		"a private note":          {fake.Call("1", ToolAsk, `{"bookingRef":"OB-BKG-1042","question":"Our DOCK 4 is booked; when?"}`), "business context"},
	}
	for name, c := range fake.AdversarialCalls {
		if name == "extra member" {
			continue // get_events declares operator here; "an undeclared member" covers it
		}
		cases["adversarial: "+name] = refusal{c, ""}
	}
	for name, rc := range cases {
		c := rc.call
		n := &node{shipments: []store.Summary{{Ref: "OB-BKG-1042", Operator: "ocean-blue.example"}}}
		m := fake.Turns(llm.GenerateResponse{ToolCalls: []llm.ToolCall{c}}, llm.GenerateResponse{Text: "OK."})
		a, tools, _ := newAssistant(m, n)
		a.Private = []string{"2026-10-16", "dock 4", " "}
		_, _ = a.Drafts.add(Draft{RequiredBy: "2026-10-15"})
		r, err := a.Turn(context.Background(), &Conversation{},
			"Look at ocean-blue.example, please, and bücher.example and my_carrier.example and cymbal-shoes.example.")
		if err != nil || len(*tools) != 1 || (*tools)[0].Error == "" || len(r.Drafts) != 0 || !strings.Contains((*tools)[0].Error, rc.why) {
			t.Errorf("%s: %+v, %v, tools %+v", name, r, err, *tools)
			continue
		}
		res := m.Calls()[1].Messages[2].ToolResults
		if len(res) != 1 || !res[0].IsError || res[0].CallID != c.ID {
			t.Errorf("%s: result %+v", name, res)
		}
		for _, p := range []string{"profile", "records", "ask", "book"} {
			if n.called(p) {
				t.Errorf("%s: the node was asked for %s", name, p)
			}
		}
	}
}

func draftJSON(override string) string {
	base := map[string]json.RawMessage{
		"operator": json.RawMessage(`"ocean-blue.example"`), "origin": json.RawMessage(`{"unLocode":"NLRTM","name":"Rotterdam"}`),
		"destination": json.RawMessage(`{"unLocode":"USEWR","name":"Newark"}`), "containerType": json.RawMessage(`"40HC"`),
		"containerCount": json.RawMessage(`1`), "cargoDescription": json.RawMessage(`"Shoes"`), "hsCode": json.RawMessage(`"6404"`),
		"earliestDeparture": json.RawMessage(`"2026-10-06"`),
	}
	var o map[string]json.RawMessage
	if err := json.Unmarshal([]byte("{"+override+"}"), &o); err != nil {
		panic(err)
	}
	for k, v := range o {
		base[k] = v
	}
	b, _ := json.Marshal(base)
	return string(b)
}

func TestDraftDetails(t *testing.T) {
	// A shipper reference, a named consignee, and no subscription endpoint.
	m := fake.Turns(llm.GenerateResponse{ToolCalls: []llm.ToolCall{fake.Call("1", ToolDraftBooking,
		draftJSON(`"shipperRef":"CY-PO-1","consigneeName":"Cymbal Footwear","consigneeDomain":"cymbal-footwear.example"`))}},
		llm.GenerateResponse{Text: "Ready."})
	a, _, _ := newAssistant(m, &node{})
	a.Self.Endpoint = ""
	r, err := a.Turn(context.Background(), &Conversation{}, "Book with OCEAN-BLUE.example. for cymbal-footwear.example")
	if err != nil || len(r.Drafts) != 1 {
		t.Fatalf("%+v, %v", r, err)
	}
	if d := r.Drafts[0]; d.Request.ShipperRef != "CY-PO-1" || d.Request.Subscription != nil || d.NotSent != nil {
		t.Errorf("draft %+v", d)
	}
	// Too many open drafts.
	for range MaxDrafts {
		_, _ = a.Drafts.add(Draft{})
	}
	if _, err := a.Drafts.add(Draft{}); err == nil {
		t.Error("over MaxDrafts")
	}
}

func TestDrafts(t *testing.T) {
	var d Drafts
	dr, _ := d.add(Draft{Operator: "o.example"})
	if _, err := d.Take("BD-9999"); !errors.Is(err, ErrNoDraft) {
		t.Errorf("Take of a missing draft: %v", err)
	}
	got, err := d.Take(dr.ID)
	if err != nil || got.Operator != "o.example" {
		t.Fatalf("Take: %+v, %v", got, err)
	}
	if _, err := d.Take(dr.ID); !errors.Is(err, ErrNoDraft) {
		t.Error("a draft taken twice")
	}
	if _, err := d.Get(dr.ID); !errors.Is(err, ErrNoDraft) {
		t.Error("a taken draft is still there")
	}
	var fresh Drafts
	fresh.Put(got)
	if _, err := fresh.Get(dr.ID); err != nil {
		t.Errorf("Put: %v", err)
	}
}

// TestRoundLimit: a model that never stops calling tools is stopped, and
// the conversation stays well formed.
func TestRoundLimit(t *testing.T) {
	m := fake.Turns(llm.GenerateResponse{ToolCalls: []llm.ToolCall{fake.Call("1", ToolList, `{}`)}})
	a, _, calls := newAssistant(m, &node{})
	a.MaxRounds = 3
	var c Conversation
	r, err := a.Turn(context.Background(), &c, "Loop")
	if err != nil || !r.Stopped || len(*calls) != 3 || !strings.Contains(r.Text, "3 rounds") {
		t.Errorf("%+v, %v, %d calls", r, err, len(*calls))
	}
	if last := c.messages[len(c.messages)-1]; last.Role != llm.Assistant || last.Text != r.Text {
		t.Errorf("last message %+v", last)
	}
	// More calls in one response than allowed: the extra ones are refused.
	var many []llm.ToolCall
	for range MaxCallsPerRound + 2 {
		many = append(many, fake.Call("x", ToolList, `{}`))
	}
	m = fake.Turns(llm.GenerateResponse{ToolCalls: many}, llm.GenerateResponse{Text: "Done."})
	a, tools, _ := newAssistant(m, &node{})
	if _, err := a.Turn(context.Background(), &Conversation{}, "Many"); err != nil {
		t.Fatal(err)
	}
	refused := 0
	for _, e := range *tools {
		if e.Error != "" {
			refused++
		}
	}
	if len(*tools) != MaxCallsPerRound+2 || refused != 2 {
		t.Errorf("%d tool events, %d refused", len(*tools), refused)
	}
}

func TestModelErrorRollsBack(t *testing.T) {
	a, _, calls := newAssistant(fake.Failing(fake.ErrUnavailable), &node{})
	var c Conversation
	if _, err := a.Turn(context.Background(), &c, "Hi"); !errors.Is(err, fake.ErrUnavailable) {
		t.Errorf("err %v", err)
	}
	if len(c.messages) != 0 || len(c.userTexts) != 0 || (*calls)[0].Outcome != "error" {
		t.Errorf("conversation of %d messages, calls %+v", len(c.messages), *calls)
	}
	for _, text := range []string{"  ", strings.Repeat("x", MaxUserText+1)} {
		if _, err := a.Turn(context.Background(), &c, text); !errors.Is(err, ErrTooLong) {
			t.Errorf("%d characters: %v", len(text), err)
		}
	}
}

func TestReplyIsCleaned(t *testing.T) {
	for reply, want := range map[string]string{
		"Late.\u001b[2J\u202e Really.\n\tYes": "Late.[2J Really.\n\tYes",
		"":                                    "(no answer)",
		"bad \xff byte":                       "bad \ufffd byte",
	} {
		a, _, _ := newAssistant(fake.Turns(llm.GenerateResponse{Text: reply}), &node{})
		if r, _ := a.Turn(context.Background(), &Conversation{}, "Hi"); r.Text != want {
			t.Errorf("%q: %q, want %q", reply, r.Text, want)
		}
	}
	long := clean(strings.Repeat("a", MaxReplyText+10))
	if !strings.HasSuffix(long, "…") || len([]rune(long)) != MaxReplyText+1 {
		t.Errorf("long reply: %d characters", len([]rune(long)))
	}
}

func TestSystemPrompt(t *testing.T) {
	a, _, _ := newAssistant(nil, &node{})
	s := a.system()
	for _, want := range []string{"Cymbal Shoes (cymbal-shoes.example)", "Today is 2026-10-01", `<skill name="freight-booking">`,
		`<skill name="shipment-tracking">`, "You can't send a booking"} {
		if !strings.Contains(s, want) {
			t.Errorf("system prompt lacks %q", want)
		}
	}
	a.Skills, a.Self.Name, a.Now = nil, "", nil
	if s := a.system(); !strings.Contains(s, "the shipper") || strings.Contains(s, "<skill") {
		t.Errorf("bare system prompt:\n%s", s)
	}
	// No tool sends a booking.
	for _, tool := range Tools {
		if strings.Contains(tool.Name, "send") || strings.Contains(tool.Name, "book") && tool.Name != ToolDraftBooking {
			t.Errorf("tool %s", tool.Name)
		}
	}
}

func TestToolResultTooLarge(t *testing.T) {
	n := &node{}
	for range 2000 {
		n.shipments = append(n.shipments, store.Summary{Ref: "OB-BKG-1", Operator: "ocean-blue.example", Status: strings.Repeat("s", 1000)})
	}
	m := fake.Turns(llm.GenerateResponse{ToolCalls: []llm.ToolCall{fake.Call("1", ToolList, `{}`)}}, llm.GenerateResponse{Text: "OK"})
	a, tools, _ := newAssistant(m, n)
	if _, err := a.Turn(context.Background(), &Conversation{}, "List"); err != nil || (*tools)[0].Error == "" {
		t.Errorf("%v, %+v", err, (*tools)[0])
	}
	// Two carriers with the same reference: say which.
	n.shipments = []store.Summary{{Ref: "R-1", Operator: "a.example"}, {Ref: "R-1", Operator: "b.example"}}
	m = fake.Turns(llm.GenerateResponse{ToolCalls: []llm.ToolCall{fake.Call("1", ToolGetEvents, `{"bookingRef":"R-1"}`)}},
		llm.GenerateResponse{ToolCalls: []llm.ToolCall{fake.Call("2", ToolGetEvents, `{"bookingRef":"R-1","operator":"b.example"}`)}},
		llm.GenerateResponse{Text: "OK"})
	a, tools, _ = newAssistant(m, n)
	if _, err := a.Turn(context.Background(), &Conversation{}, "Events"); err != nil || !strings.Contains((*tools)[0].Error, "say which carrier") ||
		(*tools)[1].Error != "" || !n.called("records b.example R-1") {
		t.Errorf("%v, %+v", err, *tools)
	}
	var rr shipper.RecordsResponse
	_ = json.Unmarshal([]byte(m.Calls()[2].Messages[4].ToolResults[0].Content), &rr)
	if len(rr.Records) != maxRecords || rr.Records[maxRecords-1].ID != "AL-6" {
		t.Errorf("%d records returned", len(rr.Records))
	}
}

func TestFakeDraftFrom(t *testing.T) {
	msgs := []llm.Message{{Role: llm.User, Text: "x"}}
	for text, want := range map[string]map[string]any{
		"Book 3 x 20ft of shoes from Hamburg to Shanghai on 2026-11-02 with ocean-blue.example, PO CY-PO-77":                        {"containerCount": 3, "containerType": "20GP", "hsCode": "6404", "earliestDeparture": "2026-11-02", "shipperRef": "CY-PO-77", "operator": "ocean-blue.example"},
		"Book two 40ft of machinery, Antwerp to Singapore, ocean-blue.example; it must arrive before the 2nd":                       {"containerCount": 2, "containerType": "40GP", "cargoDescription": "General cargo", "requiredBy": "2026-10-02"},
		"Book one 40hc, consignee is Cymbal Footwear (cymbal-footwear.example), with ocean-blue.example, Felixstowe to Los Angeles": {"operator": "ocean-blue.example", "consigneeDomain": "cymbal-footwear.example"},
	} {
		got := draftFrom(msgs, text, thursday)
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s: %s = %v, want %v", text, k, got[k], v)
			}
		}
	}
}

func TestAskLimits(t *testing.T) {
	n := &node{shipments: []store.Summary{{Ref: "A-B-1", Operator: "ocean-blue.example"}, {Ref: "A-B-2", Operator: "ocean-blue.example"},
		{Ref: "A-B-3", Operator: "ocean-blue.example"}}}
	ask := func(ref string) llm.ToolCall {
		return fake.Call("", ToolAsk, `{"bookingRef":"`+ref+`","question":"When?"}`)
	}
	m := fake.Turns(llm.GenerateResponse{ToolCalls: []llm.ToolCall{ask("A-B-1"), ask("A-B-1"), ask("A-B-2"), ask("A-B-3")}},
		llm.GenerateResponse{Text: "Done."})
	a, tools, _ := newAssistant(m, n)
	if _, err := a.Turn(context.Background(), &Conversation{}, "Ask about all of them"); err != nil {
		t.Fatal(err)
	}
	var errs []string
	for _, e := range *tools {
		errs = append(errs, e.Error)
	}
	if errs[0] != "" || !strings.Contains(errs[1], "already asked") || errs[2] != "" || !strings.Contains(errs[3], "at most 2") {
		t.Errorf("errors %q", errs)
	}
	// The calls had no IDs: each got its own, and its result names it.
	res := m.Calls()[1].Messages[2].ToolResults
	ids := map[string]bool{}
	for i, r := range res {
		ids[r.CallID] = true
		if r.CallID == "" || r.CallID != m.Calls()[1].Messages[1].ToolCalls[i].ID {
			t.Errorf("result %d: %q", i, r.CallID)
		}
	}
	if len(ids) != 4 {
		t.Errorf("call IDs %v", ids)
	}
}

// TestNormalize: an input that isn't an object is refused, and the history
// keeps {} so the next request is well formed.
func TestNormalize(t *testing.T) {
	m := fake.Turns(llm.GenerateResponse{Text: "  ", ToolCalls: []llm.ToolCall{fake.Call("a", ToolList, `[]`), fake.Call("a", ToolList, `{}`)}},
		llm.GenerateResponse{Text: "OK"})
	a, tools, _ := newAssistant(m, &node{})
	var c Conversation
	if _, err := a.Turn(context.Background(), &c, "List"); err != nil {
		t.Fatal(err)
	}
	stored := c.messages[1]
	if stored.Text != "" || string(stored.ToolCalls[0].Input) != "{}" || stored.ToolCalls[1].ID == "a" ||
		(*tools)[0].Error == "" || (*tools)[1].Error != "" {
		t.Errorf("stored %+v, tools %+v", stored, *tools)
	}
}

func TestHistoryTrim(t *testing.T) {
	m := fake.Turns(llm.GenerateResponse{ToolCalls: []llm.ToolCall{fake.Call("1", ToolList, `{}`)}}, llm.GenerateResponse{Text: "OK"})
	n := &node{}
	a, _, _ := newAssistant(m, n)
	var c Conversation
	for i := range MaxTurns + 5 {
		// Each turn: a tool call, then a reply.
		m.Respond = fake.Turns(llm.GenerateResponse{ToolCalls: []llm.ToolCall{fake.Call("1", ToolList, `{}`)}}, llm.GenerateResponse{Text: "OK"}).Respond
		if _, err := a.Turn(context.Background(), &c, "Turn "+strconv.Itoa(i)+" about ocean-blue.example"); err != nil {
			t.Fatal(err)
		}
	}
	if len(c.turns) != MaxTurns || len(c.userTexts) != MaxTurns || c.messages[0].Text != "Turn 5 about ocean-blue.example" {
		t.Errorf("%d turns, %d texts, first %q", len(c.turns), len(c.userTexts), c.messages[0].Text)
	}
	elidedCount, kept := 0, 0
	for _, msg := range c.messages {
		for _, r := range msg.ToolResults {
			if r.Content == elided {
				elidedCount++
			} else {
				kept++
			}
		}
	}
	// Trimmed as the last turn began: its results and the two before stay.
	if kept != keepResults+1 || elidedCount != MaxTurns-keepResults-1 {
		t.Errorf("%d results kept, %d elided", kept, elidedCount)
	}
	c.Reset()
	if len(c.messages) != 0 || len(c.turns) != 0 {
		t.Error("Reset kept history")
	}
}

// blocking holds the model until released.
type blocking struct{ release chan struct{} }

func (blocking) Name() string { return "blocking" }

func (b blocking) Generate(ctx context.Context, _ llm.GenerateRequest) (llm.GenerateResponse, error) {
	<-b.release
	return llm.GenerateResponse{Text: "Done."}, nil
}

func TestBusyAndPartialDrafts(t *testing.T) {
	b := blocking{release: make(chan struct{})}
	a, _, _ := newAssistant(b, &node{})
	var c Conversation
	done := make(chan error)
	go func() { _, err := a.Turn(context.Background(), &c, "Slow"); done <- err }()
	for {
		c.mu.Lock()
		busy := c.busy
		c.mu.Unlock()
		if busy {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := a.Turn(context.Background(), &c, "Again"); !errors.Is(err, ErrBusy) {
		t.Errorf("a second turn: %v", err)
	}
	close(b.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// A model error after a draft: the draft is still reported.
	n := 0
	m := &fake.Model{Respond: func(llm.GenerateRequest) (llm.GenerateResponse, error) {
		if n++; n == 1 {
			return llm.GenerateResponse{ToolCalls: []llm.ToolCall{fake.Call("1", ToolDraftBooking, draftJSON(""))}}, nil
		}
		return llm.GenerateResponse{}, fake.ErrUnavailable
	}}
	a, _, _ = newAssistant(m, &node{})
	var c2 Conversation
	r, err := a.Turn(context.Background(), &c2, "Book with ocean-blue.example")
	if !errors.Is(err, fake.ErrUnavailable) || len(r.Drafts) != 1 || len(c2.messages) != 0 || len(c2.turns) != 0 {
		t.Errorf("%+v, %v, %d messages", r, err, len(c2.messages))
	}
}

// TestStatusTool: get_status reaches only our bookings, through the node.
func TestStatusTool(t *testing.T) {
	n := &node{shipments: []store.Summary{{Ref: "OB-BKG-1042", Operator: "ocean-blue.example"}}}
	m := fake.Turns(llm.GenerateResponse{ToolCalls: []llm.ToolCall{
		fake.Call("1", ToolStatus, `{"bookingRef":"OB-BKG-1042"}`),
		fake.Call("2", ToolStatus, `{"bookingRef":"OB-BKG-9"}`)}}, llm.GenerateResponse{Text: "In transit."})
	a, tools, _ := newAssistant(m, n)
	if _, err := a.Turn(context.Background(), &Conversation{}, "Status?"); err != nil {
		t.Fatal(err)
	}
	if (*tools)[0].Error != "" || !n.called("status ocean-blue.example OB-BKG-1042") || !strings.Contains((*tools)[1].Error, "no booking OB-BKG-9") {
		t.Errorf("tools %+v, calls %v", *tools, n.calls)
	}
	res := m.Calls()[1].Messages[2].ToolResults[0].Content
	if !strings.Contains(res, `"dcsa_status":"IN_TRANSIT"`) || !strings.Contains(res, `"missing":["EV-2"]`) {
		t.Errorf("result %s", res)
	}
}

// TestStatusToolTrims: a large report reaches the model as its latest
// events (and only their missing IDs), once per booking per turn.
func TestStatusToolTrims(t *testing.T) {
	var evs []intents.TnTEventBody
	var missing []string
	for i := range 100 {
		id := fmt.Sprintf("EV-%03d", i)
		evs = append(evs, intents.TnTEventBody{EventID: id, EventType: "TRANSPORT", EventClassifierCode: "ACT", EventTypeCode: "DEPA",
			EventDateTime: "2026-10-07T18:00:00Z", Description: strings.Repeat("d", 250), UNLocationCode: "NLRTM"})
		if i%10 == 0 {
			missing = append(missing, id)
		}
	}
	got := trimStatus(&shipper.StatusResponse{Status: &intents.TnTStatus{Events: evs}, Missing: missing})
	if got.OmittedEvents != 100-maxRecords || len(got.Status.Events) != maxRecords || got.Status.Events[0].EventID != "EV-070" ||
		strings.Join(got.Missing, ",") != "EV-070,EV-080,EV-090" {
		t.Errorf("trimmed: %d omitted, %d kept, missing %v", got.OmittedEvents, len(got.Status.Events), got.Missing)
	}
	if b, _ := json.Marshal(got); len(b) > maxToolResult {
		t.Errorf("%d bytes", len(b))
	}
	n := &node{shipments: []store.Summary{{Ref: "OB-BKG-1042", Operator: "ocean-blue.example"}}}
	m := fake.Turns(llm.GenerateResponse{ToolCalls: []llm.ToolCall{
		fake.Call("1", ToolStatus, `{"bookingRef":"OB-BKG-1042"}`), fake.Call("2", ToolStatus, `{"bookingRef":"OB-BKG-1042"}`)}},
		llm.GenerateResponse{Text: "OK"})
	a, tools, _ := newAssistant(m, n)
	if _, err := a.Turn(context.Background(), &Conversation{}, "Status twice"); err != nil {
		t.Fatal(err)
	}
	if (*tools)[0].Error != "" || !strings.Contains((*tools)[1].Error, "already have") {
		t.Errorf("tools %+v", *tools)
	}
}
