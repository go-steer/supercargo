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
	"errors"
	"fmt"

	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
)

// Status is an operator's status report for a booking, with what this
// shipper missed.
type Status struct {
	Operator string
	Status   *intents.TnTStatus
	// Missing are the reported events this shipper holds no pushed copy
	// of: the pushes it missed. The report itself is signed, so they are
	// the operator's signed statement all the same.
	Missing []string
}

// Status asks operator for ref's status (ulp.ocean.tnt.request) and
// returns the verified report. It asks only about bookings this shipper
// holds, and commits to nothing.
func (s *Shipper) Status(ctx context.Context, operator, ref string) (*Status, error) {
	b, err := s.Bookings.Get(operator, ref)
	if err != nil {
		return nil, fmt.Errorf("%w: %s with %s", ErrNotOurBooking, ref, operator)
	}
	tdr := b.Confirmation.TransportDocumentReference()
	body := &intents.TnTStatusRequestBody{TnTRequest: intents.TnTRequest{TransportDocumentReference: tdr, CarrierBookingReference: ref}}
	h := envelope.NewHeader(intents.OceanTnTRequest, s.Node.Domain(), operator, s.Now())
	h.BookingRef = &ref
	resp, err := s.Node.Exchange(ctx, h, body, intents.OceanTnTResponse)
	if err != nil {
		return nil, err
	}
	r, ok := resp.Body.(*intents.TnTStatusResponseBody)
	if !ok {
		return nil, errors.New("shipper: response is not a status response")
	}
	if got := resp.Envelope.Header.BookingRef; got == nil || *got != ref {
		return nil, errors.New("shipper: the report is about another booking")
	}
	// The body must name what was asked: this booking's document, no
	// other booking, no container (none was asked about).
	if b := r.TnTResponse; b.TransportDocumentReference != tdr || (b.CarrierBookingReference != "" && b.CarrierBookingReference != ref) ||
		b.EquipmentReference != "" {
		return nil, errors.New("shipper: the report's references don't match the booking asked about")
	}
	st := &Status{Operator: operator, Status: &r.TnTResponse}
	ids := make([]string, 0, len(r.TnTResponse.Events))
	for _, e := range r.TnTResponse.Events {
		ids = append(ids, e.EventID)
	}
	held := map[string]bool{}
	for _, id := range s.held(operator, ref, ids) {
		held[id] = true
	}
	for _, id := range ids {
		if !held[id] {
			st.Missing = append(st.Missing, id)
		}
	}
	return st, nil
}
