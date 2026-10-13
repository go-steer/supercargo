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

// The road leg of the door-to-door story: Meridian books Serra Transportes
// to truck a container from the port of Santos to Cajamar.
const (
	roadRequestJSON = `{
  "shipper_reference": "MF-0007-L3",
  "shipper": {"name": "Meridian Forwarding", "domain": "meridian-forwarding.example", "identifiers": [{"scheme": "EORI", "value": "NL987654321"}]},
  "consignee": {"name": "Cymbal Calçados Ltda", "domain": "cymbal-calcados.example",
    "identifiers": [{"scheme": "BR:CNPJ", "value": "12.345.678/0001-90"}],
    "address": {"street": "Rodovia Anhanguera, km 33", "postal_code": "07750-000", "locality": "Cajamar", "region": "SP", "country": "BR"}},
  "pickup": {"facility": "Santos Brasil Tecon terminal", "facility_code": "BRSSZTCN", "un_location_code": "BRSSZ", "earliest": "2026-10-24T14:00:00Z"},
  "delivery": {"facility": "Cymbal Calçados DC, dock 2", "un_location_code": "BRSAO", "latest": "2026-10-26T12:00:00Z"},
  "containers": [{"equipment_size_type": "45G1", "count": 1, "gross_weight_kg": 12000}],
  "equipment_references": ["OCBU0000004"],
  "cargo": {"description": "Footwear (sneakers)", "hs_code": "6404"},
  "subscription": {"endpoint": "https://meridian-forwarding.example/ulp", "intents": ["ulp.tnt.event", "ulp.tnt.alert"]}
}`
	roadConfirmationJSON = `{
  "carrier_booking_reference": "SR-BKG-0913",
  "status": "confirmed",
  "planned_pickup": "2026-10-24T18:00:00Z",
  "planned_delivery": "2026-10-26T10:00:00Z",
  "fiscal_documents": [{"type": "CT-e", "access_key": "35261012345678000190570010000012341000012345"}],
  "transport_documents": [{"type": "CRT", "number": "BR-2026-0913"}],
  "subscription_id": "SUB-0913"
}`
	roadAmendmentJSON = `{"carrier_booking_reference": "SR-BKG-0913", "pickup": {"earliest": "2026-10-27T18:00:00Z"}, "delivery": {"latest": "2026-10-29T12:00:00Z"}}`
)

func roadEnvelope(intent, body string) *envelope.Envelope {
	h := HeaderFor(intent, "meridian-forwarding.example", "serra-transportes.example", now)
	if intent != RoadBookingRequest {
		h.BookingRef = ptr("SR-BKG-0913")
	}
	if intent == RoadBookingConfirmation {
		h.InReplyTo = ptr("00000000-0000-4000-8000-000000000001")
	}
	return &envelope.Envelope{Header: h, Body: json.RawMessage(body)}
}

func TestDecodeRoad(t *testing.T) {
	b, err := Decode(roadEnvelope(RoadBookingRequest, roadRequestJSON))
	if err != nil {
		t.Fatal(err)
	}
	req := b.(*RoadBookingRequestBody)
	if req.Consignee.Identifiers[0].Scheme != "BR:CNPJ" || req.Consignee.Address.Region != "SP" || req.Pickup.FacilityCode != "BRSSZTCN" {
		t.Errorf("request %+v", req)
	}
	b, err = Decode(roadEnvelope(RoadBookingConfirmation, roadConfirmationJSON))
	if err != nil {
		t.Fatal(err)
	}
	if c := b.(*RoadBookingConfirmationBody); c.FiscalDocuments[0].Type != "CT-e" || c.TransportDocuments[0].Number != "BR-2026-0913" {
		t.Errorf("confirmation %+v", c)
	}
	if _, err := Decode(roadEnvelope(RoadBookingAmendment, roadAmendmentJSON)); err != nil {
		t.Fatal(err)
	}
	rejected := `{"carrier_booking_reference":"SR-BKG-0913","status":"rejected","messages":[{"code":"no_capacity","detail":"No truck that day."}]}`
	if _, err := Decode(roadEnvelope(RoadBookingConfirmation, rejected)); err != nil {
		t.Errorf("a rejection: %v", err)
	}
	// Every scheme and document type has a valid form.
	for scheme, v := range map[string]string{"EORI": "DE123456789012345", "VAT": "NL123456789B01", "BR:CNPJ": "12345678000190",
		"BR:CPF": "123.456.789-09", "US:EIN": "12-3456789", "GLN": "8712345678906"} {
		body := strings.Replace(roadRequestJSON, `{"scheme": "EORI", "value": "NL987654321"}`, `{"scheme": "`+scheme+`", "value": "`+v+`"}`, 1)
		if _, err := Decode(roadEnvelope(RoadBookingRequest, body)); err != nil {
			t.Errorf("scheme %s: %v", scheme, err)
		}
	}
	for doc, field := range map[string]string{`{"type": "MDF-e", "access_key": "35261012345678000190580010000012341000012345"}`: "fiscal_documents",
		`{"type": "e-CMR", "number": "ECMR-NL-2026-004471"}`: "transport_documents", `{"type": "MIC/DTA", "number": "MIC-2026/77"}`: "transport_documents"} {
		body := strings.Replace(roadConfirmationJSON, `"`+field+`": [`, `"`+field+`": [`+doc+`, `, 1)
		if _, err := Decode(roadEnvelope(RoadBookingConfirmation, body)); err != nil {
			t.Errorf("%s: %v", doc, err)
		}
	}
}

func TestDecodeRoadRejects(t *testing.T) {
	type c struct{ intent, from, to string }
	R, C, A := RoadBookingRequest, RoadBookingConfirmation, RoadBookingAmendment
	for name, tc := range map[string]c{
		"an unknown member":          {R, `"cargo"`, `"x": 1, "cargo"`},
		"a dotted reference":         {R, `"MF-0007-L3"`, `"MF.0007.L3"`},
		"no shipper name":            {R, `"name": "Meridian Forwarding"`, `"name": ""`},
		"an uppercase domain":        {R, `"domain": "meridian-forwarding.example"`, `"domain": "Meridian.example"`},
		"an unknown scheme":          {R, `"scheme": "EORI"`, `"scheme": "SSN"`},
		"a malformed CNPJ":           {R, `"12.345.678/0001-90"`, `"12.345.678/0001"`},
		"a second CNPJ":              {R, `[{"scheme": "BR:CNPJ", "value": "12.345.678/0001-90"}]`, `[{"scheme": "BR:CNPJ", "value": "12.345.678/0001-90"}, {"scheme": "BR:CNPJ", "value": "12345678000190"}]`},
		"a lowercase country":        {R, `"country": "BR"`, `"country": "br"`},
		"a three-letter country":     {R, `"country": "BR"`, `"country": "BRA"`},
		"a long region":              {R, `"region": "SP"`, `"region": "SAOP"`},
		"a bad postal code":          {R, `"postal_code": "07750-000"`, `"postal_code": "-07750"`},
		"no street":                  {R, `"street": "Rodovia Anhanguera, km 33"`, `"street": ""`},
		"a stop's address abroad":    {R, `"un_location_code": "BRSAO"`, `"un_location_code": "BRSAO", "address": {"street": "Rua Augusta 1", "locality": "Lisboa", "country": "PT"}`},
		"a bad container number":     {R, `["OCBU0000004"]`, `["OCBU000004"]`},
		"more containers than asked": {R, `["OCBU0000004"]`, `["OCBU0000004", "OCBU0000005"]`},
		"a container twice": {R, `"count": 1, "gross_weight_kg": 12000}],
  "equipment_references": ["OCBU0000004"]`, `"count": 2, "gross_weight_kg": 12000}],
  "equipment_references": ["OCBU0000004", "OCBU0000004"]`},
		"a bad LOCODE":              {R, `"un_location_code": "BRSSZ"`, `"un_location_code": "Santos"`},
		"a bad facility code":       {R, `"facility_code": "BRSSZTCN"`, `"facility_code": "-TCN"`},
		"a local time":              {R, `"earliest": "2026-10-24T14:00:00Z"`, `"earliest": "2026-10-24T14:00:00-03:00"`},
		"pickup is delivery":        {R, `"facility": "Cymbal Calçados DC, dock 2", "un_location_code": "BRSAO"`, `"facility": "Santos Brasil Tecon terminal", "un_location_code": "BRSSZ"`},
		"no containers":             {R, `[{"equipment_size_type": "45G1", "count": 1, "gross_weight_kg": 12000}]`, `[]`},
		"zero weight":               {R, `"gross_weight_kg": 12000`, `"gross_weight_kg": 0`},
		"a third-party subscriber":  {R, `"endpoint": "https://meridian-forwarding.example/ulp"`, `"endpoint": "http://10.0.0.1/ulp"`},
		"an unknown status":         {C, `"status": "confirmed"`, `"status": "maybe"`},
		"no planned pickup":         {C, `"planned_pickup": "2026-10-24T18:00:00Z",`, ``},
		"delivery before pickup":    {C, `"planned_delivery": "2026-10-26T10:00:00Z"`, `"planned_delivery": "2026-10-23T10:00:00Z"`},
		"a short access key":        {C, `"35261012345678000190570010000012341000012345"`, `"3526101234"`},
		"a CT-e by number":          {C, `{"type": "CT-e", "access_key": "35261012345678000190570010000012341000012345"}`, `{"type": "CT-e", "number": "12345"}`},
		"a CT-e with both":          {C, `"access_key": "35261012345678000190570010000012341000012345"}`, `"access_key": "35261012345678000190570010000012341000012345", "number": "1"}`},
		"a CRT by access key":       {C, `{"type": "CRT", "number": "BR-2026-0913"}`, `{"type": "CRT", "access_key": "35261012345678000190570010000012341000012345"}`},
		"an unknown document":       {C, `"type": "CRT"`, `"type": "invoice"`},
		"a fiscal type misfiled":    {C, `"transport_documents": [{"type": "CRT", "number": "BR-2026-0913"}]`, `"transport_documents": [{"type": "CT-e", "access_key": "35261012345678000190570010000012341000012345"}]`},
		"a body ref not the header": {C, `"carrier_booking_reference": "SR-BKG-0913"`, `"carrier_booking_reference": "SR-BKG-0914"`},
		"a rejection with a plan":   {C, `"status": "confirmed"`, `"status": "rejected"`},
		"an amendment of nothing":   {A, `, "pickup": {"earliest": "2026-10-27T18:00:00Z"}, "delivery": {"latest": "2026-10-29T12:00:00Z"}`, ``},
		"an empty window":           {A, `"pickup": {"earliest": "2026-10-27T18:00:00Z"}`, `"pickup": {}`},
		"an inverted window":        {A, `"delivery": {"latest": "2026-10-29T12:00:00Z"}`, `"delivery": {"earliest": "2026-10-30T12:00:00Z", "latest": "2026-10-29T12:00:00Z"}`},
		"an amendment of another":   {A, `"carrier_booking_reference": "SR-BKG-0913"`, `"carrier_booking_reference": "SR-BKG-0001"`},
		"a reason in an amendment":  {A, `"pickup"`, `"reason": "vessel delayed", "pickup"`},
		"a half-punctuated CNPJ":    {R, `"12.345.678/0001-90"`, `"12345678/0001-90"`},
		"a CNPJ with letter checks": {R, `"12.345.678/0001-90"`, `"12ABC34501DE3F"`},
		"a lowercase CNPJ":          {R, `"12.345.678/0001-90"`, `"12abc34501de35"`},
		"a half-punctuated CPF":     {R, `{"scheme": "BR:CNPJ", "value": "12.345.678/0001-90"}`, `{"scheme": "BR:CPF", "value": "123.456.78909"}`},
		"a postal code ending -":    {R, `"postal_code": "07750-000"`, `"postal_code": "07750-"`},
		"another's shipper domain":  {R, `"domain": "meridian-forwarding.example"`, `"domain": "cymbal-calcados.example"`},
		"a delivery before pickup":  {R, `"latest": "2026-10-26T12:00:00Z"`, `"latest": "2026-10-24T13:00:00Z"`},
		"the pickup, renamed":       {R, `"facility": "Cymbal Calçados DC, dock 2", "un_location_code": "BRSAO"`, `"facility": " santos brasil TECON terminal", "un_location_code": "BRSSZ"`},
		"the pickup's facility":     {R, `"un_location_code": "BRSAO"`, `"un_location_code": "BRSAO", "facility_code": "brssztcn"`},
		"a document twice":          {C, `"transport_documents": [{"type": "CRT", "number": "BR-2026-0913"}]`, `"transport_documents": [{"type": "CRT", "number": "BR-2026-0913"}, {"type": "CRT", "number": "br-2026-0913"}]`},
		"letters in the key's date": {C, `"35261012345678000190570010000012341000012345"`, `"3526AB12345678000190570010000012341000012345"`},
	} {
		base := map[string]string{R: roadRequestJSON, C: roadConfirmationJSON, A: roadAmendmentJSON}[tc.intent]
		body := strings.Replace(base, tc.from, tc.to, 1)
		if body == base {
			t.Fatalf("%s: the edit didn't apply", name)
		}
		if _, err := Decode(roadEnvelope(tc.intent, body)); !errors.Is(err, ErrInvalidBody) {
			t.Errorf("%s: %v, want ErrInvalidBody", name, err)
		}
	}
	// Too many identifiers and documents.
	many := strings.Repeat(`{"scheme": "GLN", "value": "8712345678906"}, `, MaxIdentifiers)
	if _, err := Decode(roadEnvelope(R, strings.Replace(roadRequestJSON, `"identifiers": [{"scheme": "EORI"`, `"identifiers": [`+many+`{"scheme": "EORI"`, 1))); !errors.Is(err, ErrInvalidBody) {
		t.Errorf("too many identifiers: %v", err)
	}
	docs := strings.Repeat(`{"type": "CRT", "number": "X"}, `, MaxDocuments)
	if _, err := Decode(roadEnvelope(C, strings.Replace(roadConfirmationJSON, `"transport_documents": [`, `"transport_documents": [`+docs, 1))); !errors.Is(err, ErrInvalidBody) {
		t.Errorf("too many documents: %v", err)
	}
}

// TestRoadHeaders: a request has no booking yet; a confirmation answers
// something and names its booking; an amendment names its booking; all
// three name the ULP_ROAD dialect.
func TestRoadHeaders(t *testing.T) {
	for name, tc := range map[string]struct {
		intent, body string
		edit         func(*envelope.Header)
	}{
		"a request with a booking":       {RoadBookingRequest, roadRequestJSON, func(h *envelope.Header) { h.BookingRef = ptr("SR-BKG-1") }},
		"a request answering something":  {RoadBookingRequest, roadRequestJSON, func(h *envelope.Header) { h.InReplyTo = ptr("x") }},
		"a confirmation answering none":  {RoadBookingConfirmation, roadConfirmationJSON, func(h *envelope.Header) { h.InReplyTo = nil }},
		"a confirmation with no booking": {RoadBookingConfirmation, roadConfirmationJSON, func(h *envelope.Header) { h.BookingRef = nil }},
		"an amendment with no booking":   {RoadBookingAmendment, roadAmendmentJSON, func(h *envelope.Header) { h.BookingRef = nil }},
		"an amendment answering":         {RoadBookingAmendment, roadAmendmentJSON, func(h *envelope.Header) { h.InReplyTo = ptr("x") }},
		"no dialect":                     {RoadBookingRequest, roadRequestJSON, func(h *envelope.Header) { h.Dialect = nil }},
		"the ocean dialect":              {RoadBookingRequest, roadRequestJSON, func(h *envelope.Header) { h.Dialect = &DialectDCSAEBL }},
		"another dialect version": {RoadBookingAmendment, roadAmendmentJSON, func(h *envelope.Header) {
			h.Dialect = &envelope.Dialect{Standard: "ULP_ROAD", Version: "9"}
		}},
	} {
		e := roadEnvelope(tc.intent, tc.body)
		tc.edit(&e.Header)
		if _, err := Decode(e); !errors.Is(err, ErrInvalidBody) {
			t.Errorf("%s: %v, want ErrInvalidBody", name, err)
		}
	}
	if d, ok := DialectFor(RoadBookingRequest); !ok || d != DialectULPRoad {
		t.Errorf("DialectFor(road request) = %+v, %v", d, ok)
	}
}

// TestDerivedFrom: a re-issued event or alert says where it came from;
// a signed source names its ID, an unsigned one what it was.
func TestDerivedFrom(t *testing.T) {
	ok := []string{
		`[{"id": "EV-000003", "signed": true}]`,
		`[{"source": "EDIFACT IFTSTA from polder-haulage.example", "reference": "PH4471-1", "signed": false}]`,
		`[{"id": "AL-6", "signed": true}, {"source": "X12 214", "reference": "000000001", "signed": false}]`,
	}
	bad := map[string]string{
		"signed without an id":    `[{"source": "x", "signed": true}]`,
		"unsigned without source": `[{"reference": "PH4471-1", "signed": false}]`,
		"a dotted id":             `[{"id": "EV.1", "signed": true}]`,
		"a multiline source":      `[{"source": "a\nb", "signed": false}]`,
		"a bad reference":         `[{"source": "x", "reference": "-r", "signed": false}]`,
		"an unknown member":       `[{"id": "EV-1", "signed": true, "trust": "high"}]`,
		"too many sources":        "[" + strings.Repeat(`{"id": "EV-1", "signed": true}, `, MaxCitations) + `{"id": "EV-2", "signed": true}]`,
		"a string, not an object": `["EV-000003"]`,
		"signed as a string":      `[{"id": "EV-1", "signed": "true"}]`,
		"signed left out (false)": `[{"id": "EV-1"}]`, // then it needs a source
		"an unsigned source's id": `[{"id": "EV-1", "source": "X12 214", "signed": false}]`,
		"the same source twice":   `[{"id": "EV-1", "signed": true}, {"id": "EV-1", "signed": true}]`,
		"a reference with a path": `[{"source": "x", "reference": "PH/4471", "signed": false}]`,
	}
	for _, intent := range []string{TnTEvent, TnTAlert} {
		base := map[string]string{TnTEvent: eventJSON, TnTAlert: alertJSON}[intent]
		with := func(df string) string { return strings.Replace(base, `{`, `{"derived_from": `+df+`, `, 1) }
		for _, df := range ok {
			if _, err := Decode(push(intent, with(df))); err != nil {
				t.Errorf("%s %s: %v", intent, df, err)
			}
		}
		for name, df := range bad {
			if _, err := Decode(push(intent, with(df))); !errors.Is(err, ErrInvalidBody) {
				t.Errorf("%s %s: %v, want ErrInvalidBody", intent, name, err)
			}
		}
	}
}

// TestRoadEdges: the rest of the rules, and each body's intent.
func TestRoadEdges(t *testing.T) {
	if (&RoadBookingRequestBody{}).Intent() != RoadBookingRequest || (&RoadBookingConfirmationBody{}).Intent() != RoadBookingConfirmation ||
		(&RoadBookingAmendmentBody{}).Intent() != RoadBookingAmendment {
		t.Error("a road body names the wrong intent")
	}
	lines := strings.Repeat(`{"equipment_size_type": "45G1", "count": 1, "gross_weight_kg": 12000}, `, MaxEquipmentLines)
	for name, edit := range map[string][2]string{
		"a malformed latest":       {`"latest": "2026-10-26T12:00:00Z"`, `"latest": "the 26th"`},
		"an empty facility":        {`"facility": "Santos Brasil Tecon terminal"`, `"facility": ""`},
		"a bad address on a stop":  {`"un_location_code": "BRSAO"`, `"un_location_code": "BRSAO", "address": {"street": "", "locality": "Cajamar", "country": "BR"}`},
		"a consignee without name": {`"name": "Cymbal Calçados Ltda"`, `"name": ""`},
		"too many container lines": {`"containers": [`, `"containers": [` + lines},
	} {
		body := strings.Replace(roadRequestJSON, edit[0], edit[1], 1)
		if body == roadRequestJSON {
			t.Fatalf("%s: the edit didn't apply", name)
		}
		if _, err := Decode(roadEnvelope(RoadBookingRequest, body)); !errors.Is(err, ErrInvalidBody) {
			t.Errorf("%s: %v, want ErrInvalidBody", name, err)
		}
	}
}

// TestRoadIdentifiers: the forms a Brazilian identifier takes, including
// the alphanumeric CNPJ (from July 2026) and an access key that carries one.
func TestRoadIdentifiers(t *testing.T) {
	for _, v := range []string{"12.345.678/0001-90", "12345678000190", "12ABC34501DE35", "12.ABC.345/01DE-35"} {
		body := strings.Replace(roadRequestJSON, `"12.345.678/0001-90"`, `"`+v+`"`, 1)
		if _, err := Decode(roadEnvelope(RoadBookingRequest, body)); err != nil {
			t.Errorf("CNPJ %s: %v", v, err)
		}
	}
	for _, v := range []string{"123.456.789-09", "12345678909"} {
		body := strings.Replace(roadRequestJSON, `{"scheme": "BR:CNPJ", "value": "12.345.678/0001-90"}`, `{"scheme": "BR:CPF", "value": "`+v+`"}`, 1)
		if _, err := Decode(roadEnvelope(RoadBookingRequest, body)); err != nil {
			t.Errorf("CPF %s: %v", v, err)
		}
	}
	key := "352610" + "12ABC34501DE35" + "570010000012341000012345"
	body := strings.Replace(roadConfirmationJSON, `"35261012345678000190570010000012341000012345"`, `"`+key+`"`, 1)
	if _, err := Decode(roadEnvelope(RoadBookingConfirmation, body)); err != nil {
		t.Errorf("an access key with an alphanumeric CNPJ: %v", err)
	}
	// A request without the shipper's domain is fine: the sender is the shipper.
	body = strings.Replace(roadRequestJSON, `"domain": "meridian-forwarding.example", `, ``, 1)
	if _, err := Decode(roadEnvelope(RoadBookingRequest, body)); err != nil {
		t.Errorf("a shipper without a domain: %v", err)
	}
}
