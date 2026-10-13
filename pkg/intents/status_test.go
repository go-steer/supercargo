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

const (
	statusRequestJSON  = `{"tnt_request":{"transport_document_reference":"OB-BKG-1042","equipment_reference":null,"carrier_booking_reference":"OB-BKG-1042"}}`
	statusResponseJSON = `{"tnt_response":{"transport_document_reference":"OB-BKG-1042","carrier_booking_reference":"OB-BKG-1042",
"equipment_reference":null,"dcsa_status":"IN_TRANSIT","vessel_name":"Ocean Blue Aurora","vessel_imo_number":"9619907",
"voyage_number":"42W","port_of_loading":"NLRTM","port_of_discharge":"USEWR",
"current_location":{"un_location_code":"NLRTM","facility_name":"Port of Rotterdam"},
"estimated_time_of_arrival":"2026-10-16T08:00:00Z","events":[` + eventJSON + `]}}`
)

func statusEnvelope(intent, body string) *envelope.Envelope {
	e := push(intent, body)
	if intent == OceanTnTResponse {
		e.Header.InReplyTo = ptr(e.Header.MessageID)
	}
	return e
}

func TestDecodeStatus(t *testing.T) {
	b, err := Decode(statusEnvelope(OceanTnTRequest, statusRequestJSON))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if q := b.(*TnTStatusRequestBody); q.TnTRequest.TransportDocumentReference != "OB-BKG-1042" || q.Intent() != OceanTnTRequest {
		t.Errorf("request %+v", q)
	}
	b, err = Decode(statusEnvelope(OceanTnTResponse, statusResponseJSON))
	if err != nil {
		t.Fatalf("response: %v", err)
	}
	if r := b.(*TnTStatusResponseBody); r.TnTResponse.DCSAStatus != StatusInTransit || len(r.TnTResponse.Events) != 1 || r.Intent() != OceanTnTResponse {
		t.Errorf("response %+v", r)
	}
	minimal := `{"tnt_response":{"transport_document_reference":"X","dcsa_status":"NOT_FOUND","events":[]}}`
	if _, err := Decode(statusEnvelope(OceanTnTResponse, minimal)); err != nil {
		t.Errorf("NOT_FOUND: %v", err)
	}
	// Both carry the DCSA Track & Trace dialect, and the request may carry
	// a receiver's delegation (milestone 7).
	e := statusEnvelope(OceanTnTRequest, statusRequestJSON)
	e.Header.Dialect = nil
	if _, err := Decode(e); !errors.Is(err, ErrInvalidBody) {
		t.Errorf("a request without its dialect: %v", err)
	}
	e = statusEnvelope(OceanTnTRequest, statusRequestJSON)
	e.Attachments.Delegation = json.RawMessage(`{}`)
	if _, err := Decode(e); err != nil {
		t.Errorf("a request with a delegation: %v", err)
	}
	e = statusEnvelope(OceanTnTResponse, statusResponseJSON)
	e.Header.InReplyTo = nil
	if _, err := Decode(e); !errors.Is(err, ErrInvalidBody) {
		t.Errorf("a response that answers nothing: %v", err)
	}
}

func TestStatusRejects(t *testing.T) {
	events := make([]string, MaxStatusEvents+1)
	for i := range events {
		events[i] = strings.Replace(eventJSON, `"EV-0014"`, `"EV-`+strings.Repeat("1", 1)+string(rune('A'+i%26))+string(rune('A'+i/26))+`"`, 1)
	}
	for name, c := range map[string]struct{ intent, body string }{
		"request: no document":              {OceanTnTRequest, `{"tnt_request":{"carrier_booking_reference":"X"}}`},
		"request: bad document":             {OceanTnTRequest, `{"tnt_request":{"transport_document_reference":"OB/1"}}`},
		"request: long document":            {OceanTnTRequest, `{"tnt_request":{"transport_document_reference":"` + strings.Repeat("A", 65) + `"}}`},
		"request: long container":           {OceanTnTRequest, `{"tnt_request":{"transport_document_reference":"X","equipment_reference":"` + strings.Repeat("A", 33) + `"}}`},
		"request: unwrapped":                {OceanTnTRequest, `{"transport_document_reference":"X"}`},
		"request: unknown member":           {OceanTnTRequest, `{"tnt_request":{"transport_document_reference":"X","shipper_name":"Acme"}}`},
		"response: unknown status":          {OceanTnTResponse, strings.Replace(statusResponseJSON, `"IN_TRANSIT"`, `"LOST"`, 1)},
		"response: bad imo":                 {OceanTnTResponse, strings.Replace(statusResponseJSON, `"9619907"`, `"IMO9619907"`, 1)},
		"response: bad port":                {OceanTnTResponse, strings.Replace(statusResponseJSON, `"port_of_loading":"NLRTM"`, `"port_of_loading":"Rotterdam"`, 1)},
		"response: bad eta":                 {OceanTnTResponse, strings.Replace(statusResponseJSON, `"2026-10-16T08:00:00Z"`, `"soon"`, 1)},
		"response: bad location":            {OceanTnTResponse, strings.Replace(statusResponseJSON, `{"un_location_code":"NLRTM"`, `{"un_location_code":"x"`, 1)},
		"response: open location":           {OceanTnTResponse, strings.Replace(statusResponseJSON, `"facility_name":"Port of Rotterdam"}`, `"altitude":12}`, 1)},
		"response: bad facility code":       {OceanTnTResponse, strings.Replace(statusResponseJSON, `"facility_name":"Port of Rotterdam"}`, `"facility_code":"DELTA TERM"}`, 1)},
		"response: long facility code":      {OceanTnTResponse, strings.Replace(statusResponseJSON, `"facility_name":"Port of Rotterdam"}`, `"facility_code":"`+strings.Repeat("A", 33)+`"}`, 1)},
		"response: lat without lng":         {OceanTnTResponse, strings.Replace(statusResponseJSON, `"facility_name":"Port of Rotterdam"}`, `"lat":51.9}`, 1)},
		"response: lng without lat":         {OceanTnTResponse, strings.Replace(statusResponseJSON, `"facility_name":"Port of Rotterdam"}`, `"lng":4.1}`, 1)},
		"response: lat out of range":        {OceanTnTResponse, strings.Replace(statusResponseJSON, `"facility_name":"Port of Rotterdam"}`, `"lat":91,"lng":4.1}`, 1)},
		"response: lng out of range":        {OceanTnTResponse, strings.Replace(statusResponseJSON, `"facility_name":"Port of Rotterdam"}`, `"lat":51.9,"lng":-180.5}`, 1)},
		"response: lat as a string":         {OceanTnTResponse, strings.Replace(statusResponseJSON, `"facility_name":"Port of Rotterdam"}`, `"lat":"51.9","lng":4.1}`, 1)},
		"response: huge lat":                {OceanTnTResponse, strings.Replace(statusResponseJSON, `"facility_name":"Port of Rotterdam"}`, `"lat":1e400,"lng":4.1}`, 1)},
		"response: no events":               {OceanTnTResponse, `{"tnt_response":{"transport_document_reference":"X","dcsa_status":"BOOKED"}}`},
		"response: bad event":               {OceanTnTResponse, strings.Replace(statusResponseJSON, `"LOAD"`, `"LOST"`, 1)},
		"response: repeated event":          {OceanTnTResponse, strings.Replace(statusResponseJSON, eventJSON, eventJSON+","+eventJSON, 1)},
		"response: too many events":         {OceanTnTResponse, strings.Replace(statusResponseJSON, eventJSON, strings.Join(events, ","), 1)},
		"response: hidden vessel":           {OceanTnTResponse, strings.Replace(statusResponseJSON, `"vessel_name":"Ocean Blue Aurora"`, `"vessel_name":"Aurora\u202e"`, 1)},
		"response: NOT_FOUND with events":   {OceanTnTResponse, strings.Replace(statusResponseJSON, `"IN_TRANSIT"`, `"NOT_FOUND"`, 1)},
		"response: NOT_FOUND with a vessel": {OceanTnTResponse, `{"tnt_response":{"transport_document_reference":"X","dcsa_status":"NOT_FOUND","vessel_name":"Aurora","events":[]}}`},
		"response: bad voyage":              {OceanTnTResponse, strings.Replace(statusResponseJSON, `"voyage_number":"42W"`, `"voyage_number":"4 2"`, 1)},
	} {
		if b, err := Decode(statusEnvelope(c.intent, c.body)); !errors.Is(err, ErrInvalidBody) {
			t.Errorf("%s: Decode = %+v, %v; want ErrInvalidBody", name, b, err)
		}
	}
}

// TestBookingRefQueryable: every carrier booking reference a confirmation
// may carry can be asked about in a status request; references we can't
// query are refused when confirmed, not discovered later.
func TestBookingRefQueryable(t *testing.T) {
	for _, r := range []string{"OB-BKG-1042", "OB.BKG.1042", "a", "A_1-b", strings.Repeat("A", 64), "OB/1", "-x", "_x", "x y", "é"} {
		conf := strings.Replace(confirmationJSON, `"carrier_booking_reference": "OB-BKG-1042"`, `"carrier_booking_reference": "`+r+`"`, 1)
		env := confirmation(conf)
		env.Header.BookingRef = ptr(r)
		_, confErr := Decode(env)
		req := `{"tnt_request":{"transport_document_reference":"OB-TD-1","carrier_booking_reference":"` + r + `"}}`
		_, reqErr := Decode(statusEnvelope(OceanTnTRequest, req))
		if confErr == nil && reqErr != nil {
			t.Errorf("%q is a valid booking reference in a confirmation but can't be asked about: %v", r, reqErr)
		}
	}
	// The case that differed: a dot.
	env := confirmation(strings.Replace(confirmationJSON, `"carrier_booking_reference": "OB-BKG-1042"`, `"carrier_booking_reference": "OB.BKG.1042"`, 1))
	env.Header.BookingRef = ptr("OB.BKG.1042")
	if _, err := Decode(env); !errors.Is(err, ErrInvalidBody) || !strings.Contains(err.Error(), "carrier_booking_reference") {
		t.Errorf("a booking reference with a dot: %v, want ErrInvalidBody on carrier_booking_reference", err)
	}
	// A reference starts with a letter or digit.
	for _, r := range []string{"-OB-1042", "_OB-1042"} {
		env := confirmation(strings.Replace(confirmationJSON, `"carrier_booking_reference": "OB-BKG-1042"`, `"carrier_booking_reference": "`+r+`"`, 1))
		env.Header.BookingRef = ptr(r)
		if _, err := Decode(env); !errors.Is(err, ErrInvalidBody) || !strings.Contains(err.Error(), "carrier_booking_reference") {
			t.Errorf("a booking reference starting %q: %v, want ErrInvalidBody on carrier_booking_reference", r[:1], err)
		}
	}
}

// TestCurrentLocationFull: the members the ULP payload specification's own
// status responses fill (facility code, coordinates) are accepted.
func TestCurrentLocationFull(t *testing.T) {
	full := strings.Replace(statusResponseJSON, `{"un_location_code":"NLRTM","facility_name":"Port of Rotterdam"}`,
		`{"un_location_code":"NLRTM","facility_code":"DELTA1","facility_name":"Delta Terminal","lat":51.9563,"lng":4.0461}`, 1)
	b, err := Decode(statusEnvelope(OceanTnTResponse, full))
	if err != nil {
		t.Fatal(err)
	}
	c := b.(*TnTStatusResponseBody).TnTResponse.CurrentLocation
	if c.FacilityCode != "DELTA1" || c.Lat == nil || *c.Lat != 51.9563 || c.Lng == nil || *c.Lng != 4.0461 {
		t.Errorf("current_location %+v", c)
	}
	for _, edge := range []string{`"lat":90,"lng":180`, `"lat":-90,"lng":-180`, `"lat":0,"lng":0`} {
		in := strings.Replace(statusResponseJSON, `"facility_name":"Port of Rotterdam"}`, edge+`}`, 1)
		if _, err := Decode(statusEnvelope(OceanTnTResponse, in)); err != nil {
			t.Errorf("%s: %v", edge, err)
		}
	}
}
