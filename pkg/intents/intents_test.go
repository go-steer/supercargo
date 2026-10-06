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

package intents

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/supercargo/pkg/envelope"
)

var now = time.Date(2026, 10, 9, 14, 3, 11, 0, time.UTC)

func ptr(s string) *string { return &s }

// bookingRequestJSON is the DESIGN.md §A.5 example.
const bookingRequestJSON = `{
  "shipperRef": "CY-PO-55120",
  "origin":      { "unLocode": "NLRTM", "name": "Rotterdam" },
  "destination": { "unLocode": "USEWR", "name": "Newark" },
  "equipment":   [{ "isoType": "45G1", "label": "40HC", "count": 1 }],
  "cargo":       { "description": "Footwear (sneakers)", "hsCode": "6404" },
  "earliestDeparture": "2026-10-06",
  "consignee":   { "name": "Cymbal Footwear", "domain": "cymbal-footwear.example" },
  "subscription": {
    "endpoint": "https://cymbal-shoes.example/ulp",
    "intents":  ["ulp.tnt.event", "ulp.tnt.alert"]
  }
}`

const confirmationJSON = `{
  "bookingRef": "OB-BKG-1042",
  "status": "confirmed",
  "vessel": { "name": "Ocean Blue Aurora", "voyage": "42W" },
  "plannedDeparture": "2026-10-07T18:00:00Z",
  "plannedArrival":   "2026-10-13T08:00:00Z",
  "equipmentRefs": ["OCBU1234565"],
  "subscriptionId": "SUB-311",
  "messages": []
}`

func request(body string) *envelope.Envelope {
	return &envelope.Envelope{
		Header: envelope.NewHeader(BookingRequest, "cymbal-shoes.example", "ocean-blue.example", now),
		Body:   json.RawMessage(body),
	}
}

func confirmation(body string) *envelope.Envelope {
	h := request(bookingRequestJSON).Header.Reply(BookingConfirmation, now)
	h.BookingRef = ptr("OB-BKG-1042")
	return &envelope.Envelope{Header: h, Body: json.RawMessage(body)}
}

func errorEnvelope(body string) *envelope.Envelope {
	h := request(bookingRequestJSON).Header.Reply(Error, now)
	return &envelope.Envelope{Header: h, Body: json.RawMessage(body)}
}

func TestDecodeBookingRequest(t *testing.T) {
	b, err := Decode(request(bookingRequestJSON))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	r, ok := b.(*BookingRequestBody)
	if !ok || r.Intent() != BookingRequest {
		t.Fatalf("Decode = %T", b)
	}
	if r.ShipperRef != "CY-PO-55120" || r.Equipment[0].Count != 1 || r.Consignee.Domain != "cymbal-footwear.example" ||
		r.Subscription.EndpointHost() != "cymbal-shoes.example" {
		t.Errorf("decoded %+v", r)
	}
	// The optional parts are optional.
	minimal := `{"shipperRef":"P1","origin":{"unLocode":"NLRTM","name":"R"},"destination":{"unLocode":"USEWR","name":"N"},` +
		`"equipment":[{"isoType":"22G1","label":"20GP","count":2}],"cargo":{"description":"Shoes","hsCode":"6404"},"earliestDeparture":"2026-10-06"}`
	if _, err := Decode(request(minimal)); err != nil {
		t.Errorf("Decode(minimal): %v", err)
	}
}

func TestDecodeConfirmationAndError(t *testing.T) {
	b, err := Decode(confirmation(confirmationJSON))
	if err != nil {
		t.Fatalf("Decode(confirmation): %v", err)
	}
	if c := b.(*BookingConfirmationBody); c.Vessel.Voyage != "42W" || c.Intent() != BookingConfirmation {
		t.Errorf("decoded %+v", c)
	}
	if _, err := Decode(confirmation(`{"bookingRef":"OB-BKG-1042","status":"rejected","messages":[{"code":"vessel_full","detail":"No space on 42W"}]}`)); err != nil {
		t.Errorf("Decode(rejected): %v", err)
	}
	b, err = Decode(errorEnvelope(`{"code":"forbidden","detail":"Sender is not a party to OB-BKG-1042","retryable":false}`))
	if err != nil {
		t.Fatalf("Decode(error): %v", err)
	}
	if e := b.(*ErrorBody); e.Code != CodeForbidden || e.Intent() != Error {
		t.Errorf("decoded %+v", e)
	}
	withRef := errorEnvelope(`{"code":"not_found","detail":"No such booking","retryable":false}`)
	withRef.Header.BookingRef = ptr("OB-BKG-1")
	if _, err := Decode(withRef); err != nil {
		t.Errorf("an error may name a booking: %v", err)
	}
}

func TestSupported(t *testing.T) {
	for _, in := range []string{BookingRequest, BookingConfirmation, Error} {
		if !Supported(in) {
			t.Errorf("Supported(%s) = false", in)
		}
	}
	for _, in := range []string{TnTEvent, TnTInquiry, "ulp.nope", ""} {
		if Supported(in) {
			t.Errorf("Supported(%s) = true", in)
		}
	}
}

func TestDecodeUnsupported(t *testing.T) {
	e := request(`{}`)
	e.Header.Intent = TnTEvent
	if _, err := Decode(e); !errors.Is(err, ErrUnsupportedIntent) {
		t.Errorf("Decode(%s) = %v, want ErrUnsupportedIntent", TnTEvent, err)
	}
}

// TestHeaderRules covers each intent's rules for bookingRef, inReplyTo and
// the delegation attachment.
func TestHeaderRules(t *testing.T) {
	for _, tc := range []struct {
		name string
		e    func() *envelope.Envelope
	}{
		{"request with bookingRef", func() *envelope.Envelope {
			e := request(bookingRequestJSON)
			e.Header.BookingRef = ptr("OB-1")
			return e
		}},
		{"request with inReplyTo", func() *envelope.Envelope {
			e := request(bookingRequestJSON)
			e.Header.InReplyTo = ptr(e.Header.MessageID)
			return e
		}},
		{"request with delegation", func() *envelope.Envelope {
			e := request(bookingRequestJSON)
			e.Attachments.Delegation = json.RawMessage(`{}`)
			return e
		}},
		{"confirmation without bookingRef", func() *envelope.Envelope {
			e := confirmation(confirmationJSON)
			e.Header.BookingRef = nil
			return e
		}},
		{"confirmation without inReplyTo", func() *envelope.Envelope {
			e := confirmation(confirmationJSON)
			e.Header.InReplyTo = nil
			return e
		}},
		{"confirmation with delegation", func() *envelope.Envelope {
			e := confirmation(confirmationJSON)
			e.Attachments.Delegation = json.RawMessage(`{}`)
			return e
		}},
		{"confirmation for another booking", func() *envelope.Envelope {
			e := confirmation(confirmationJSON)
			e.Header.BookingRef = ptr("OB-BKG-9999")
			return e
		}},
		{"error with delegation", func() *envelope.Envelope {
			e := errorEnvelope(`{"code":"internal","detail":"Try later","retryable":true}`)
			e.Attachments.Delegation = json.RawMessage(`{}`)
			return e
		}},
		{"error without inReplyTo", func() *envelope.Envelope {
			e := errorEnvelope(`{"code":"internal","detail":"Try later","retryable":true}`)
			e.Header.InReplyTo = nil
			return e
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if b, err := Decode(tc.e()); !errors.Is(err, ErrInvalidBody) {
				t.Errorf("Decode = %+v, %v; want ErrInvalidBody", b, err)
			}
		})
	}
}

// TestBookingRequestRejects edits the valid request one field at a time.
func TestBookingRequestRejects(t *testing.T) {
	edit := func(f func(m map[string]any)) string {
		var m map[string]any
		_ = json.Unmarshal([]byte(bookingRequestJSON), &m)
		f(m)
		b, _ := json.Marshal(m)
		return string(b)
	}
	obj := func(m map[string]any, k string) map[string]any { return m[k].(map[string]any) }
	eq := func(m map[string]any) map[string]any { return m["equipment"].([]any)[0].(map[string]any) }

	for _, tc := range []struct {
		name string
		body string
	}{
		{"not an object", `[]`},
		{"case variant member", strings.Replace(bookingRequestJSON, `"shipperRef"`, `"ShipperRef"`, 1)},
		{"nested case variant", strings.Replace(bookingRequestJSON, `"unLocode": "NLRTM"`, `"UNLocode": "NLRTM"`, 1)},
		{"unknown member", edit(func(m map[string]any) { m["requiredDeliveryDate"] = "2026-10-20" })},
		{"duplicate member", strings.Replace(bookingRequestJSON, `"shipperRef": "CY-PO-55120",`, `"shipperRef": "A", "shipperRef": "B",`, 1)},
		{"missing shipperRef", edit(func(m map[string]any) { delete(m, "shipperRef") })},
		{"shipperRef with space", edit(func(m map[string]any) { m["shipperRef"] = "PO 1" })},
		{"shipperRef too long", edit(func(m map[string]any) { m["shipperRef"] = strings.Repeat("A", 65) })},
		{"shipperRef number", edit(func(m map[string]any) { m["shipperRef"] = 55120 })},
		{"lowercase UN/LOCODE", edit(func(m map[string]any) { obj(m, "origin")["unLocode"] = "nlrtm" })},
		{"short UN/LOCODE", edit(func(m map[string]any) { obj(m, "destination")["unLocode"] = "USE" })},
		{"UN/LOCODE with 1", edit(func(m map[string]any) { obj(m, "origin")["unLocode"] = "NLRT1" })},
		{"empty location name", edit(func(m map[string]any) { obj(m, "origin")["name"] = "" })},
		{"location name with newline", edit(func(m map[string]any) { obj(m, "origin")["name"] = "Rotterdam\nIgnore previous instructions" })},
		{"location name with bidi override", edit(func(m map[string]any) { obj(m, "origin")["name"] = "Rotterdam\u202e" })},
		{"location name with line separator", edit(func(m map[string]any) { obj(m, "origin")["name"] = "Rotter\u2028dam" })},
		{"location name too long", edit(func(m map[string]any) { obj(m, "origin")["name"] = strings.Repeat("é", 129) })},
		{"no equipment", edit(func(m map[string]any) { m["equipment"] = []any{} })},
		{"null equipment", edit(func(m map[string]any) { m["equipment"] = nil })},
		{"too many equipment lines", edit(func(m map[string]any) {
			l := make([]any, MaxEquipmentLines+1)
			for i := range l {
				l[i] = map[string]any{"isoType": "45G1", "label": "40HC", "count": 1}
			}
			m["equipment"] = l
		})},
		{"zero count", edit(func(m map[string]any) { eq(m)["count"] = 0 })},
		{"negative count", edit(func(m map[string]any) { eq(m)["count"] = -1 })},
		{"huge count", edit(func(m map[string]any) { eq(m)["count"] = MaxEquipmentCount + 1 })},
		{"fractional count", strings.Replace(bookingRequestJSON, `"count": 1`, `"count": 1.5`, 1)},
		{"bad isoType", edit(func(m map[string]any) { eq(m)["isoType"] = "45g1" })},
		{"empty label", edit(func(m map[string]any) { eq(m)["label"] = "" })},
		{"long label", edit(func(m map[string]any) { eq(m)["label"] = strings.Repeat("L", 17) })},
		{"bad hsCode", edit(func(m map[string]any) { obj(m, "cargo")["hsCode"] = "64.04" })},
		{"empty description", edit(func(m map[string]any) { obj(m, "cargo")["description"] = "" })},
		{"long description", edit(func(m map[string]any) { obj(m, "cargo")["description"] = strings.Repeat("d", 513) })},
		{"bad date", edit(func(m map[string]any) { m["earliestDeparture"] = "06/10/2026" })},
		{"impossible date", edit(func(m map[string]any) { m["earliestDeparture"] = "2026-02-30" })},
		{"timestamp for date", edit(func(m map[string]any) { m["earliestDeparture"] = "2026-10-06T00:00:00Z" })},
		{"consignee without domain", edit(func(m map[string]any) { delete(obj(m, "consignee"), "domain") })},
		{"consignee bad domain", edit(func(m map[string]any) { obj(m, "consignee")["domain"] = "Cymbal Footwear" })},
		{"consignee empty name", edit(func(m map[string]any) { obj(m, "consignee")["name"] = "" })},
		{"endpoint ftp", edit(func(m map[string]any) { obj(m, "subscription")["endpoint"] = "ftp://cymbal-shoes.example/ulp" })},
		{"endpoint relative", edit(func(m map[string]any) { obj(m, "subscription")["endpoint"] = "/ulp" })},
		{"endpoint without host", edit(func(m map[string]any) { obj(m, "subscription")["endpoint"] = "https:///ulp" })},
		{"endpoint with userinfo", edit(func(m map[string]any) {
			obj(m, "subscription")["endpoint"] = "https://ocean-blue.example@evil.example/ulp"
		})},
		{"endpoint with fragment", edit(func(m map[string]any) { obj(m, "subscription")["endpoint"] = "https://cymbal-shoes.example/ulp#x" })},
		{"endpoint with query", edit(func(m map[string]any) { obj(m, "subscription")["endpoint"] = "https://cymbal-shoes.example/ulp?to=x" })},
		{"endpoint opaque", edit(func(m map[string]any) { obj(m, "subscription")["endpoint"] = "https:cymbal-shoes.example" })},
		{"endpoint uppercase host", edit(func(m map[string]any) { obj(m, "subscription")["endpoint"] = "https://Cymbal-Shoes.example/ulp" })},
		{"endpoint with port", edit(func(m map[string]any) { obj(m, "subscription")["endpoint"] = "https://cymbal-shoes.example:6379/ulp" })},
		{"endpoint with empty port", edit(func(m map[string]any) { obj(m, "subscription")["endpoint"] = "https://cymbal-shoes.example:/ulp" })},
		{"endpoint IP literal", edit(func(m map[string]any) { obj(m, "subscription")["endpoint"] = "https://10.0.0.5/ulp" })},
		{"endpoint IPv6 literal", edit(func(m map[string]any) { obj(m, "subscription")["endpoint"] = "https://[::1]/ulp" })},
		{"endpoint trailing dot", edit(func(m map[string]any) { obj(m, "subscription")["endpoint"] = "https://cymbal-shoes.example./ulp" })},
		{"endpoint single label", edit(func(m map[string]any) { obj(m, "subscription")["endpoint"] = "https://localhost/ulp" })},
		{"description with tag characters", edit(func(m map[string]any) {
			obj(m, "cargo")["description"] = "Shoes\U000E0049\U000E0067\U000E006E\U000E006F\U000E0072\U000E0065"
		})},
		{"description with zero-width space", edit(func(m map[string]any) { obj(m, "cargo")["description"] = "Sho\u200bes" })},
		{"description with BOM", edit(func(m map[string]any) { obj(m, "cargo")["description"] = "\ufeffShoes" })},
		{"description with soft hyphen", edit(func(m map[string]any) { obj(m, "cargo")["description"] = "Sho\u00ades" })},
		{"description with private use", edit(func(m map[string]any) { obj(m, "cargo")["description"] = "Shoes\ue000" })},
		{"description with noncharacter", edit(func(m map[string]any) { obj(m, "cargo")["description"] = "Shoes\ufdd0" })},
		{"shipperRef with slash", edit(func(m map[string]any) { m["shipperRef"] = "PO/../../x" })},
		{"endpoint too long", edit(func(m map[string]any) {
			obj(m, "subscription")["endpoint"] = "https://cymbal-shoes.example/" + strings.Repeat("p", 500)
		})},
		{"no subscribed intents", edit(func(m map[string]any) { obj(m, "subscription")["intents"] = []any{} })},
		{"unsubscribable intent", edit(func(m map[string]any) { obj(m, "subscription")["intents"] = []any{"ulp.booking.request"} })},
		{"duplicate subscribed intent", edit(func(m map[string]any) {
			obj(m, "subscription")["intents"] = []any{"ulp.tnt.event", "ulp.tnt.event"}
		})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if b, err := Decode(request(tc.body)); !errors.Is(err, ErrInvalidBody) {
				t.Errorf("Decode = %+v, %v; want ErrInvalidBody", b, err)
			}
		})
	}
}

func TestConfirmationRejects(t *testing.T) {
	edit := func(f func(m map[string]any)) string {
		var m map[string]any
		_ = json.Unmarshal([]byte(confirmationJSON), &m)
		f(m)
		b, _ := json.Marshal(m)
		return string(b)
	}
	for _, tc := range []struct {
		name string
		body string
	}{
		{"unknown status", edit(func(m map[string]any) { m["status"] = "maybe" })},
		{"uppercase status", edit(func(m map[string]any) { m["status"] = "CONFIRMED" })},
		{"confirmed without vessel", edit(func(m map[string]any) { delete(m, "vessel") })},
		{"confirmed without departure", edit(func(m map[string]any) { delete(m, "plannedDeparture") })},
		{"confirmed without arrival", edit(func(m map[string]any) { delete(m, "plannedArrival") })},
		{"arrival before departure", edit(func(m map[string]any) { m["plannedArrival"] = "2026-10-06T08:00:00Z" })},
		{"arrival equals departure", edit(func(m map[string]any) { m["plannedArrival"] = "2026-10-07T18:00:00Z" })},
		{"departure with offset", edit(func(m map[string]any) { m["plannedDeparture"] = "2026-10-07T20:00:00+02:00" })},
		{"departure with fractional seconds", edit(func(m map[string]any) { m["plannedDeparture"] = "2026-10-07T18:00:00.5Z" })},
		{"rejected with bad time", edit(func(m map[string]any) { m["status"] = "rejected"; m["plannedArrival"] = "soon" })},
		{"bad bookingRef", edit(func(m map[string]any) { m["bookingRef"] = "OB BKG" })},
		{"vessel without voyage", edit(func(m map[string]any) { m["vessel"] = map[string]any{"name": "Aurora"} })},
		{"vessel empty name", edit(func(m map[string]any) { m["vessel"] = map[string]any{"name": "", "voyage": "42W"} })},
		{"bad container number", edit(func(m map[string]any) { m["equipmentRefs"] = []any{"OCBU-1234565"} })},
		{"too many container numbers", edit(func(m map[string]any) {
			l := make([]any, MaxEquipmentLines*MaxEquipmentCount+1)
			for i := range l {
				l[i] = "OCBU1234565"
			}
			m["equipmentRefs"] = l
		})},
		{"bad subscriptionId", edit(func(m map[string]any) { m["subscriptionId"] = "SUB 311" })},
		{"message bad code", edit(func(m map[string]any) { m["messages"] = []any{map[string]any{"code": "Bad Code", "detail": "x"}} })},
		{"message empty detail", edit(func(m map[string]any) { m["messages"] = []any{map[string]any{"code": "c", "detail": ""}} })},
		{"too many messages", edit(func(m map[string]any) {
			l := make([]any, MaxMessages+1)
			for i := range l {
				l[i] = map[string]any{"code": "c", "detail": "d"}
			}
			m["messages"] = l
		})},
		{"case variant", strings.Replace(confirmationJSON, `"bookingRef"`, `"BookingRef"`, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if b, err := Decode(confirmation(tc.body)); !errors.Is(err, ErrInvalidBody) {
				t.Errorf("Decode = %+v, %v; want ErrInvalidBody", b, err)
			}
		})
	}
}

func TestErrorRejects(t *testing.T) {
	for _, body := range []string{
		`{"code":"teapot","detail":"x","retryable":false}`,
		`{"code":"FORBIDDEN","detail":"x","retryable":false}`,
		`{"code":"forbidden","detail":"","retryable":false}`,
		`{"code":"forbidden","retryable":false}`,
		`{"code":"forbidden","detail":"` + strings.Repeat("x", 513) + `","retryable":false}`,
		`{"code":"forbidden","detail":"line\nbreak","retryable":false}`,
		`{"code":"forbidden","detail":"x","retryable":"no"}`,
		`{"code":"forbidden","detail":"x","retryable":false,"trace":"goroutine 1"}`,
	} {
		if b, err := Decode(errorEnvelope(body)); !errors.Is(err, ErrInvalidBody) {
			t.Errorf("Decode(%s) = %+v, %v; want ErrInvalidBody", body, b, err)
		}
	}
}

func TestEndpointHost(t *testing.T) {
	for in, want := range map[string]string{
		"https://cymbal-shoes.example/ulp":      "cymbal-shoes.example",
		"https://cymbal-shoes.example:8443/ulp": "cymbal-shoes.example",
		"http://[::1]:80/ulp":                   "::1",
		"%zz":                                   "",
	} {
		if got := (&Subscription{Endpoint: in}).EndpointHost(); got != want {
			t.Errorf("EndpointHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTextAllowsMultiline(t *testing.T) {
	if err := text("q", "line one\n\tline two", 100, true); err != nil {
		t.Errorf("multiline text rejected: %v", err)
	}
	if err := text("q", "bell\a", 100, true); err == nil {
		t.Error("control character accepted in multiline text")
	}
}
