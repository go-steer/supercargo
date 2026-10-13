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

	"github.com/go-steer/supercargo/pkg/envelope"
)

// The order of the door-to-door story: Cymbal Shoes asks Meridian to move a
// container from its Tilburg warehouse to Cymbal Calçados' Cajamar DC.
const (
	forwardingRequestJSON = `{
  "shipper_reference": "CY-PO-55120",
  "incoterms": {"rule": "DAP", "version": "2020", "named_place": "Cymbal Calçados DC, Cajamar"},
  "shipper": {"name": "Cymbal Shoes B.V.", "domain": "cymbal-shoes.example", "identifiers": [{"scheme": "EORI", "value": "NL123456789"}]},
  "consignee": {"name": "Cymbal Calçados Ltda", "domain": "cymbal-calcados.example", "identifiers": [{"scheme": "BR:CNPJ", "value": "12.345.678/0001-90"}]},
  "place_of_receipt": {"type": "door", "un_location_code": "NLTLB", "earliest": "2026-10-06T06:00:00Z",
    "address": {"street": "Ringbaan Oost 100", "postal_code": "5013 CD", "locality": "Tilburg", "country": "NL"}},
  "place_of_delivery": {"type": "door", "facility": "Cymbal Calçados DC, dock 2", "un_location_code": "BRSAO", "latest": "2026-10-27T12:00:00Z",
    "address": {"street": "Rodovia Anhanguera, km 33", "postal_code": "07750-000", "locality": "Cajamar", "region": "SP", "country": "BR"}},
  "containers": [{"equipment_size_type": "45G1", "count": 1, "gross_weight_kg": 12000}],
  "cargo": {"description": "Footwear (sneakers)", "hs_code": "6404"},
  "subscription": {"endpoint": "https://cymbal-shoes.example/ulp", "intents": ["ulp.tnt.event", "ulp.tnt.alert"]}
}`
	forwardingConfirmationJSON = `{
  "forwarder_reference": "MF-0007",
  "status": "confirmed",
  "legs": [
    {"mode": "road", "from": "NLTLB", "to": "NLRTM", "planned_departure": "2026-10-06T08:00:00Z", "planned_arrival": "2026-10-06T11:00:00Z"},
    {"mode": "ocean", "from": "NLRTM", "to": "BRSSZ", "carrier": "Ocean Blue", "vessel": "Ocean Blue Aurora 42W", "planned_departure": "2026-10-07T18:00:00Z", "planned_arrival": "2026-10-23T08:00:00Z"},
    {"mode": "road", "from": "BRSSZ", "to": "BRSAO", "carrier": "Serra Transportes", "planned_departure": "2026-10-24T18:00:00Z", "planned_arrival": "2026-10-26T10:00:00Z"}
  ],
  "planned_delivery": "2026-10-26T10:00:00Z",
  "subscription_id": "SUB-0007"
}`
)

func forwardingEnvelope(intent, body string) *envelope.Envelope {
	h := HeaderFor(intent, "cymbal-shoes.example", "meridian-forwarding.example", now)
	if intent == ForwardingConfirmation {
		h.Sender, h.Receiver = h.Receiver, h.Sender
		h.BookingRef = ptr("MF-0007")
		h.InReplyTo = ptr("00000000-0000-4000-8000-000000000001")
	}
	return &envelope.Envelope{Header: h, Body: json.RawMessage(body)}
}

// TestDecodeForwarding: the order and its confirmation decode, and a
// rejection carries nothing else.
func TestDecodeForwarding(t *testing.T) {
	b, err := Decode(forwardingEnvelope(ForwardingRequest, forwardingRequestJSON))
	if err != nil {
		t.Fatal(err)
	}
	if r := b.(*ForwardingRequestBody); r.Incoterms.Rule != "DAP" || r.PlaceOfDelivery.Address.Region != "SP" || r.Consignee.Identifiers[0].Scheme != "BR:CNPJ" {
		t.Errorf("request %+v", r)
	}
	b, err = Decode(forwardingEnvelope(ForwardingConfirmation, forwardingConfirmationJSON))
	if err != nil {
		t.Fatal(err)
	}
	if c := b.(*ForwardingConfirmationBody); len(c.Legs) != 3 || c.Legs[1].Vessel != "Ocean Blue Aurora 42W" {
		t.Errorf("confirmation %+v", c)
	}
	if _, err := Decode(forwardingEnvelope(ForwardingConfirmation, `{"forwarder_reference": "MF-0007", "status": "rejected", "messages": [{"code": "no_capacity", "detail": "No space this month"}]}`)); err != nil {
		t.Errorf("a rejection: %v", err)
	}
	// A port-to-door order, legs without times, and a train.
	port := strings.Replace(forwardingRequestJSON, `"type": "door", "un_location_code": "NLTLB"`, `"type": "port", "facility": "Euromax terminal", "facility_code": "NLRTMEMX", "un_location_code": "NLRTM"`, 1)
	if _, err := Decode(forwardingEnvelope(ForwardingRequest, port)); err != nil {
		t.Errorf("port to door: %v", err)
	}
	bare := `{"forwarder_reference": "MF-0008", "status": "confirmed", "planned_delivery": "2026-10-26T10:00:00Z",
	  "legs": [{"mode": "rail", "from": "NLRTM", "to": "DEDUI"}, {"mode": "road", "from": "DEDUI", "to": "DEDUI"}]}`
	if _, err := Decode(forwardingEnvelope(ForwardingConfirmation, strings.Replace(bare, "MF-0008", "MF-0007", 1))); err != nil {
		t.Errorf("legs without times: %v", err)
	}
}

// TestDecodeForwardingRejects: every rule, one broken at a time.
func TestDecodeForwardingRejects(t *testing.T) {
	const R, C = ForwardingRequest, ForwardingConfirmation
	for name, tc := range map[string]struct{ intent, from, to string }{
		"no incoterms rule":        {R, `"rule": "DAP"`, `"rule": ""`},
		"an unknown rule":          {R, `"rule": "DAP"`, `"rule": "DAT"`}, // Incoterms 2010, gone in 2020
		"another version":          {R, `"version": "2020"`, `"version": "2010"`},
		"no named place":           {R, `"named_place": "Cymbal Calçados DC, Cajamar"`, `"named_place": ""`},
		"another's shipper domain": {R, `"domain": "cymbal-shoes.example"`, `"domain": "mallory.example"`},
		"a consignee without name": {R, `"name": "Cymbal Calçados Ltda"`, `"name": ""`},
		"a bad consignee CNPJ":     {R, `"12.345.678/0001-90"`, `"12.345.678/0001"`},
		"a door without an address": {R, `"un_location_code": "NLTLB", "earliest": "2026-10-06T06:00:00Z",
    "address": {"street": "Ringbaan Oost 100", "postal_code": "5013 CD", "locality": "Tilburg", "country": "NL"}}`, `"un_location_code": "NLTLB"}`},
		"a port without a terminal": {R, `"type": "door", "un_location_code": "NLTLB"`, `"type": "port", "un_location_code": "NLTLB"`},
		"an unknown place type":     {R, `"type": "door", "un_location_code": "NLTLB"`, `"type": "airport", "un_location_code": "NLTLB"`},
		"a bad LOCODE":              {R, `"un_location_code": "NLTLB"`, `"un_location_code": "Tilburg"`},
		"a bad facility code":       {R, `"facility": "Cymbal Calçados DC, dock 2",`, `"facility": "Cymbal Calçados DC, dock 2", "facility_code": "-2",`},
		"a multiline facility":      {R, `"facility": "Cymbal Calçados DC, dock 2"`, `"facility": "dock\n2"`},
		"a bad address":             {R, `"country": "NL"`, `"country": "nl"`},
		"delivered where received":  {R, `"street": "Rodovia Anhanguera, km 33", "postal_code": "07750-000", "locality": "Cajamar", "region": "SP", "country": "BR"`, `"street": "ringbaan  oost 100", "postal_code": "5013 cd", "locality": "TILBURG", "country": "NL"`},
		"due before it's ready":     {R, `"latest": "2026-10-27T12:00:00Z"`, `"latest": "2026-10-05T12:00:00Z"`},
		"a local time":              {R, `"earliest": "2026-10-06T06:00:00Z"`, `"earliest": "2026-10-06T08:00:00+02:00"`},
		"no containers":             {R, `[{"equipment_size_type": "45G1", "count": 1, "gross_weight_kg": 12000}]`, `[]`},
		"a bad container line":      {R, `"count": 1`, `"count": 0`},
		"no cargo description":      {R, `"description": "Footwear (sneakers)"`, `"description": ""`},
		"a third-party subscriber":  {R, `"endpoint": "https://cymbal-shoes.example/ulp"`, `"endpoint": "http://10.0.0.1/ulp"`},
		"an unknown member":         {R, `"shipper_reference"`, `"priority": "high", "shipper_reference"`},
		"a dotted reference":        {R, `"CY-PO-55120"`, `"CY.PO.55120"`},
		"no legs":                   {C, forwardingConfirmationJSON[strings.Index(forwardingConfirmationJSON, `"legs"`):strings.Index(forwardingConfirmationJSON, `"planned_delivery"`)], ``},
		"no planned delivery":       {C, `"planned_delivery": "2026-10-26T10:00:00Z",`, ``},
		"a bad planned delivery":    {C, `"planned_delivery": "2026-10-26T10:00:00Z"`, `"planned_delivery": "the 26th"`},
		"an unknown status":         {C, `"status": "confirmed"`, `"status": "maybe"`},
		"a rejection with a plan":   {C, `"status": "confirmed"`, `"status": "rejected"`},
		"a gap between legs":        {C, `"from": "BRSSZ", "to": "BRSAO"`, `"from": "BRRIG", "to": "BRSAO"`},
		"an unknown mode":           {C, `"mode": "ocean"`, `"mode": "air"`},
		"a truck with a vessel":     {C, `"mode": "road", "from": "BRSSZ", "to": "BRSAO", "carrier": "Serra Transportes"`, `"mode": "road", "from": "BRSSZ", "to": "BRSAO", "carrier": "Serra Transportes", "vessel": "Aurora"`},
		"a ship going nowhere":      {C, `"mode": "ocean", "from": "NLRTM", "to": "BRSSZ"`, `"mode": "ocean", "from": "NLRTM", "to": "NLRTM"`},
		"a leg arriving first":      {C, `"planned_arrival": "2026-10-23T08:00:00Z"`, `"planned_arrival": "2026-10-07T08:00:00Z"`},
		"departing before arriving": {C, `"planned_departure": "2026-10-24T18:00:00Z"`, `"planned_departure": "2026-10-22T18:00:00Z"`},
		"arriving after delivery":   {C, `"planned_delivery": "2026-10-26T10:00:00Z"`, `"planned_delivery": "2026-10-25T10:00:00Z"`},
		"a bad leg LOCODE":          {C, `"from": "NLTLB"`, `"from": "Tilburg"`},
		"a multiline carrier":       {C, `"carrier": "Ocean Blue"`, `"carrier": "Ocean\nBlue"`},
		"a multiline vessel":        {C, `"vessel": "Ocean Blue Aurora 42W"`, `"vessel": "Aurora\n42W"`},
		"a body ref not the header": {C, `"forwarder_reference": "MF-0007"`, `"forwarder_reference": "MF-0008"`},
		"a bad subscription id":     {C, `"SUB-0007"`, `"SUB.0007"`},
		"a reason for the plan":     {C, `"status"`, `"margin": "12%", "status"`},
	} {
		base := map[string]string{R: forwardingRequestJSON, C: forwardingConfirmationJSON}[tc.intent]
		body := strings.Replace(base, tc.from, tc.to, 1)
		if body == base {
			t.Fatalf("%s: the edit didn't apply", name)
		}
		if _, err := Decode(forwardingEnvelope(tc.intent, body)); !errors.Is(err, ErrInvalidBody) {
			t.Errorf("%s: %v, want ErrInvalidBody", name, err)
		}
	}
	// Too many legs.
	var ls []string
	for range MaxLegs + 1 {
		ls = append(ls, `{"mode": "road", "from": "NLTLB", "to": "NLTLB"}`)
	}
	many := `{"forwarder_reference": "MF-0007", "status": "confirmed", "planned_delivery": "2026-10-26T10:00:00Z", "legs": [` + strings.Join(ls, ", ") + `]}`
	if _, err := Decode(forwardingEnvelope(C, many)); !errors.Is(err, ErrInvalidBody) {
		t.Errorf("too many legs: %v", err)
	}
}

// TestForwardingHeaders: an order has no reference yet; a confirmation
// answers it and names the order; neither carries a dialect.
func TestForwardingHeaders(t *testing.T) {
	for name, tc := range map[string]struct {
		intent, body string
		edit         func(*envelope.Header)
	}{
		"an order with a reference":     {ForwardingRequest, forwardingRequestJSON, func(h *envelope.Header) { h.BookingRef = ptr("MF-0007") }},
		"an order answering something":  {ForwardingRequest, forwardingRequestJSON, func(h *envelope.Header) { h.InReplyTo = ptr("x") }},
		"an order with a dialect":       {ForwardingRequest, forwardingRequestJSON, func(h *envelope.Header) { h.Dialect = &DialectULPRoad }},
		"a confirmation answering none": {ForwardingConfirmation, forwardingConfirmationJSON, func(h *envelope.Header) { h.InReplyTo = nil }},
		"a confirmation with no order":  {ForwardingConfirmation, forwardingConfirmationJSON, func(h *envelope.Header) { h.BookingRef = nil }},
		"a confirmation with a dialect": {ForwardingConfirmation, forwardingConfirmationJSON, func(h *envelope.Header) { h.Dialect = &DialectDCSATnT }},
	} {
		e := forwardingEnvelope(tc.intent, tc.body)
		tc.edit(&e.Header)
		if _, err := Decode(e); !errors.Is(err, ErrInvalidBody) {
			t.Errorf("%s: %v, want ErrInvalidBody", name, err)
		}
	}
	if (&ForwardingRequestBody{}).Intent() != ForwardingRequest || (&ForwardingConfirmationBody{}).Intent() != ForwardingConfirmation {
		t.Error("a forwarding body names the wrong intent")
	}
	if _, ok := DialectFor(ForwardingRequest); ok {
		t.Error("the order has a dialect")
	}
}

// TestSamePlace: two places are the same by facility code, by facility at
// one UN/LOCODE, or by address; two doors in one town are not.
func TestSamePlace(t *testing.T) {
	addr := func(street string) *Address {
		return &Address{Street: street, Locality: "Tilburg", Country: "NL"}
	}
	for name, tc := range map[string]struct {
		a, b Place
		same bool
	}{
		"one facility code":     {Place{FacilityCode: "NLRTMEMX", UNLocationCode: "NLRTM"}, Place{FacilityCode: "nlrtmemx", UNLocationCode: "NLRTM"}, true},
		"one terminal":          {Place{Facility: "Euromax", UNLocationCode: "NLRTM"}, Place{Facility: " euromax ", UNLocationCode: "NLRTM"}, true},
		"one address":           {Place{UNLocationCode: "NLTLB", Address: addr("Ringbaan Oost 100")}, Place{UNLocationCode: "NLTLB", Address: addr("ringbaan oost  100")}, true},
		"two doors in one town": {Place{FacilityCode: "W1", UNLocationCode: "NLTLB", Address: addr("Ringbaan Oost 100")}, Place{UNLocationCode: "NLTLB", Address: addr("Ringbaan West 2")}, false},
		"two unnamed doors":     {Place{UNLocationCode: "NLTLB"}, Place{UNLocationCode: "NLTLB"}, false},
		"one name, two towns":   {Place{Facility: "DC", UNLocationCode: "NLTLB"}, Place{Facility: "DC", UNLocationCode: "NLRTM"}, false},
	} {
		if got := samePlace(tc.a, tc.b); got != tc.same {
			t.Errorf("%s: %v, want %v", name, got, tc.same)
		}
	}
}
