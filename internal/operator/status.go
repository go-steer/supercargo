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
	"slices"

	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/intents"
)

// StatusService answers ulp.ocean.tnt.request (docs/DESIGN.md, A.5): a
// shipment's DCSA status and its events, from the booking's record and
// the operator's own log. Deterministic: no model is involved. It lets a
// subscriber catch up on pushes it missed.
//
// Authorization is the inquiry's: only the booking's shipper may ask (a
// receiver's delegation is milestone 7, and refused until then), decided
// from the booking's parties before anything else is loaded.
type StatusService struct {
	Bookings *store.Bookings
	Log      *store.EventLog // the operator's published events and alerts
}

// Authorize implements node.Handler.
func (s *StatusService) Authorize(_ context.Context, req *node.Request) error {
	h := req.Envelope.Header
	if len(req.Envelope.Attachments.Delegation) > 0 {
		return node.Reject(intents.CodeForbidden, "Delegated access is not supported yet.", "status request carries a delegation")
	}
	b, err := s.Bookings.Get(h.Receiver, *h.BookingRef)
	if err != nil {
		return &node.Rejection{Code: notAParty.Code, Detail: notAParty.Detail, Reason: "no booking " + *h.BookingRef}
	}
	if b.Shipper != h.Sender {
		return &node.Rejection{Code: notAParty.Code, Detail: notAParty.Detail, Reason: h.Sender + " is not the shipper of " + b.Ref}
	}
	return nil
}

// Handle implements node.Handler. The request's references must name the
// booking in the header (its eBL's transport document reference, and its
// carrier booking reference if given); otherwise the answer is NOT_FOUND.
func (s *StatusService) Handle(_ context.Context, req *node.Request) (*node.Reply, error) {
	h := req.Envelope.Header
	b, err := s.Bookings.Get(h.Receiver, *h.BookingRef)
	if err != nil {
		return nil, err // authorized a moment ago; bookings are never removed
	}
	q := req.Body.(*intents.TnTStatusRequestBody).TnTRequest
	resp := intents.TnTStatus{TransportDocumentReference: q.TransportDocumentReference, CarrierBookingReference: q.CarrierBookingReference,
		EquipmentReference: q.EquipmentReference, Events: []intents.TnTEventBody{}}
	reply := func() (*node.Reply, error) {
		return &node.Reply{Body: &intents.TnTStatusResponseBody{TnTResponse: resp}, BookingRef: b.Ref}, nil
	}
	if q.TransportDocumentReference != b.Confirmation.TransportDocumentReference() || (q.CarrierBookingReference != "" && q.CarrierBookingReference != b.Ref) ||
		(q.EquipmentReference != "" && !slices.Contains(b.Confirmation.EquipmentRefs(), q.EquipmentReference)) {
		resp.DCSAStatus = intents.StatusNotFound
		return reply()
	}
	recs := s.Log.List(b.Operator, b.Ref)
	sum := store.Summarize(b, recs)
	resp.CarrierBookingReference = b.Ref
	resp.PortOfLoading, resp.PortOfDischarge = b.Confirmation.EBL.PortOfLoading, b.Confirmation.EBL.PortOfDischarge
	resp.VesselName, resp.VoyageNumber = sum.Vessel, sum.Voyage
	resp.EstimatedTimeOfArrival = sum.ETA
	resp.DCSAStatus = intents.StatusBooked
	for _, r := range recs {
		if r.Intent != intents.TnTEvent {
			continue
		}
		var e intents.TnTEventBody
		if json.Unmarshal(r.Body, &e) != nil {
			continue // the operator's own, validated when published
		}
		if q.EquipmentReference != "" && e.EquipmentReference != "" && e.EquipmentReference != q.EquipmentReference {
			continue // another container's
		}
		resp.Events = append(resp.Events, e)
		if e.EventClassifierCode == intents.ClassifierActual {
			if st, ok := dcsaStatus[e.EventTypeCode]; ok {
				resp.DCSAStatus = st
			}
			// Where the cargo is: a shipment event's location is a
			// document's, not the cargo's.
			if e.EventType != intents.EventTypeShipment {
				resp.CurrentLocation = &intents.CurrentLocation{UNLocationCode: e.UNLocationCode, FacilityName: e.FacilityName}
			}
		}
	}
	if n := len(resp.Events); n > intents.MaxStatusEvents {
		resp.Events = resp.Events[n-intents.MaxStatusEvents:] // the latest
	}
	// The signed reply must fit an envelope (base64 makes the payload
	// grow by a third): drop the oldest events until the body fits.
	for len(resp.Events) > 0 {
		b, err := json.Marshal(intents.TnTStatusResponseBody{TnTResponse: resp})
		if err != nil || len(b) <= maxStatusBody {
			break
		}
		resp.Events = resp.Events[1:]
	}
	return reply()
}

// maxStatusBody bounds a status report's JSON, leaving room in the
// 128 KiB envelope for the header and the base64 encoding.
const maxStatusBody = 88 << 10

// dcsaStatus is the shipment status an actual event moves it to.
var dcsaStatus = map[string]string{
	"GTIN": intents.StatusReceived,
	"LOAD": intents.StatusLoaded,
	"DEPA": intents.StatusInTransit,
	"ARRI": intents.StatusInTransit, // arrived, not yet discharged
	"DISC": intents.StatusDischarged,
	"GTOT": intents.StatusDelivered,
}
