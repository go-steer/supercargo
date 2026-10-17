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
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/go-steer/supercargo/internal/backend"
	"github.com/go-steer/supercargo/internal/docks"
	"github.com/go-steer/supercargo/internal/llm"
)

// The receiving site's dock schedule (internal/docks): the shipper's own
// assistant reads a shipment's appointment and the free slots, and drafts
// moving it when a delay calls for it; a person confirms the move in its
// dialog. The schedule is ours: none of it goes to another company.

// docksPrompt is the dock schedule's rules, after the shipper's.
const docksPrompt = `
- Our receiving site's dock appointments are in our dock scheduling system: get_dock_appointment reads a shipment's (by our reference), find_dock_slots lists the free slots. When a delay moves a delivery past its appointment, offer to move it: pick a free slot after the new delivery estimate, with a few hours' margin, and draft_dock_move prepares the move; the user confirms it in a dialog, and only then is it moved. Say which system it is (the appointment's source). Appointments are ours: never put them in a message to another company.`

// Docks is the dock schedule through the node: only the dialog's send
// moves an appointment.
type Docks interface {
	DockAppointment(ctx context.Context, ref string) (*backend.Appointment, error)
	DockSlots(ctx context.Context, from time.Time, days int) ([]backend.DockSlot, error)
	MoveDock(ctx context.Context, id string, from time.Time, to backend.DockSlot) (*backend.Appointment, error)
}

// DockAppointment implements Docks.
func (n ControlNode) DockAppointment(ctx context.Context, ref string) (*backend.Appointment, error) {
	var r docks.AppointmentResponse
	if err := n.do(ctx, "GET", "/docks/appointments/"+url.PathEscape(ref), nil, &r); err != nil {
		return nil, withMessage(err, r.Error)
	}
	return r.Appointment, nil
}

// DockSlots implements Docks.
func (n ControlNode) DockSlots(ctx context.Context, from time.Time, days int) ([]backend.DockSlot, error) {
	var r docks.SlotsResponse
	q := url.Values{"from": {from.Format(time.DateOnly)}, "days": {strconv.Itoa(days)}}
	if err := n.do(ctx, "GET", "/docks/slots?"+q.Encode(), nil, &r); err != nil {
		return nil, withMessage(err, r.Error)
	}
	return r.Slots, nil
}

// MoveDock implements Docks.
func (n ControlNode) MoveDock(ctx context.Context, id string, from time.Time, to backend.DockSlot) (*backend.Appointment, error) {
	var r docks.AppointmentResponse
	if err := n.do(ctx, "POST", "/docks/appointments/"+url.PathEscape(id)+"/move", docks.MoveRequest{From: from, Dock: to.Dock, Start: to.Start, End: to.End}, &r); err != nil {
		return nil, withMessage(err, r.Error)
	}
	return r.Appointment, nil
}

// The dock tools' names.
const (
	ToolGetDock   = "get_dock_appointment"
	ToolFindSlots = "find_dock_slots"
	ToolDraftDock = "draft_dock_move"
)

// maxSlots bounds the slots find_dock_slots returns.
const maxSlots = 40

// DockTools are the shipper's own assistant's, with a dock schedule.
var DockTools = []llm.Tool{
	{Name: ToolGetDock, Description: "Our receiving site's dock appointment for one of our shipments, by our reference (shipperRef), or for a truck we follow by delegation, by the carrier's booking reference: its ID, dock, start and end (local time with offset), and the scheduling system it's in. Ours: never sent to another company.",
		Schema: json.RawMessage(`{"type":"object","properties":{"shipperRef":{"type":"string","maxLength":64},"operator":{"type":"string","maxLength":253,"description":"A followed truck's carrier: only needed if the reference is ambiguous"}},"required":["shipperRef"],"additionalProperties":false}`)},
	{Name: ToolFindSlots, Description: "Free dock slots at our receiving site from a day (YYYY-MM-DD), for 1-7 days, earliest first: dock, start and end.",
		Schema: json.RawMessage(`{"type":"object","properties":{"from":{"type":"string","maxLength":10},"days":{"type":"integer","minimum":1,"maximum":7}},"required":["from","days"],"additionalProperties":false}`)},
	{Name: ToolDraftDock, Description: "Prepare moving one of our shipments' dock appointment to a free slot (from find_dock_slots: its dock and start, as given). Nothing moves: the user reviews it and confirms it themselves; only then is the appointment moved.",
		Schema: json.RawMessage(`{"type":"object","properties":{"shipperRef":{"type":"string","maxLength":64},"operator":{"type":"string","maxLength":253,"description":"A followed truck's carrier: only needed if the reference is ambiguous"},"dock":{"type":"string","maxLength":32},"start":{"type":"string","maxLength":40,"description":"The slot's start, as find_dock_slots gave it (RFC 3339)"}},"required":["shipperRef","dock","start"],"additionalProperties":false}`)},
}

// DockDraft is a dock appointment's move, prepared: from where, to where.
type DockDraft struct {
	AppointmentID string    `json:"appointmentId"`
	ShipperRef    string    `json:"shipperRef"`
	Site          string    `json:"site"`
	Source        string    `json:"source"`
	WasDock       string    `json:"wasDock"`
	WasStart      time.Time `json:"wasStart"`
	WasEnd        time.Time `json:"wasEnd"`
	Dock          string    `json:"dock"`
	Start         time.Time `json:"start"`
	End           time.Time `json:"end"`
}

// dockRef is the schedule's reference for ref: a truck we follow by
// delegation goes by its carrier and reference (docks.TruckKey), one of
// our own shipments by our reference. A reference that could be either,
// or two carriers' trucks, needs the carrier (operator) named.
func (a *Assistant) dockRef(ctx context.Context, ref, operator string) (string, error) {
	all, err := a.Node.Shipments(ctx)
	if err != nil {
		return "", err
	}
	var trucks []string
	own := false
	for _, s := range all {
		switch {
		case s.DelegatedBy != "" && s.Ref == ref && (operator == "" || s.Operator == operator):
			trucks = append(trucks, docks.TruckKey(s.Operator, s.Ref))
		case s.DelegatedBy == "" && s.ShipperRef == ref:
			own = true
		}
	}
	switch {
	case len(trucks) > 1:
		return "", refuse("two carriers' trucks are %s: say which carrier (operator)", ref)
	case len(trucks) == 1 && own && operator == "":
		return "", refuse("%s is both one of our shipments and a truck we follow: for the truck, say its carrier (operator)", ref)
	case len(trucks) == 1:
		return trucks[0], nil
	}
	return ref, nil
}

func (a *Assistant) runDocks(ctx context.Context, call llm.ToolCall) (any, error) {
	switch call.Name {
	case ToolGetDock:
		var in struct{ ShipperRef, Operator string }
		_ = json.Unmarshal(call.Input, &in)
		key, err := a.dockRef(ctx, in.ShipperRef, in.Operator)
		if err != nil {
			return nil, err
		}
		appt, err := a.Docks.DockAppointment(ctx, key)
		if err != nil {
			return nil, err
		}
		a.learned(appt)
		return appt, nil
	case ToolFindSlots:
		var in struct {
			From string
			Days int
		}
		_ = json.Unmarshal(call.Input, &in)
		from, err := time.Parse(time.DateOnly, in.From)
		if err != nil || in.Days < 1 || in.Days > 7 {
			return nil, refuse("from is a YYYY-MM-DD day and days 1-7")
		}
		s, err := a.Docks.DockSlots(ctx, from, in.Days)
		if err != nil {
			return nil, err
		}
		more := len(s) > maxSlots
		if more {
			s = s[:maxSlots]
		}
		return map[string]any{"slots": s, "more": more}, nil
	case ToolDraftDock:
		return a.draftDock(ctx, call.Input)
	}
	return nil, fmt.Errorf("%w: %s", llm.ErrUnknownTool, call.Name)
}

func (a *Assistant) draftDock(ctx context.Context, raw json.RawMessage) (any, error) {
	var in struct{ ShipperRef, Operator, Dock, Start string }
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, refuse("%v", err)
	}
	start, err := time.Parse(time.RFC3339, in.Start)
	if err != nil {
		return nil, refuse("start %q is not an RFC 3339 time from find_dock_slots", in.Start)
	}
	key, err := a.dockRef(ctx, in.ShipperRef, in.Operator)
	if err != nil {
		return nil, err
	}
	appt, err := a.Docks.DockAppointment(ctx, key)
	if err != nil {
		return nil, err
	}
	a.learned(appt)
	// The slot's day at the site (the appointment's offset), whatever
	// offset the model wrote it in.
	slots, err := a.Docks.DockSlots(ctx, start.In(appt.Start.Location()), 1)
	if err != nil {
		return nil, err
	}
	for _, s := range slots {
		if s.Dock == in.Dock && s.Start.Equal(start) {
			dd := &DockDraft{AppointmentID: appt.ID, ShipperRef: appt.ShipperRef, Site: appt.Site, Source: appt.Source,
				WasDock: appt.Dock, WasStart: appt.Start, WasEnd: appt.End, Dock: s.Dock, Start: s.Start, End: s.End}
			dr, err := a.Drafts.add(Draft{Dock: dd, Created: a.now()})
			if err != nil {
				return nil, refuse("%v", err)
			}
			return dr.Result(), nil
		}
	}
	return nil, refuse("%s at %s isn't a free slot: find_dock_slots lists them", in.Dock, in.Start)
}

// sendDock moves a drafted appointment through the node.
func (a *Assistant) sendDock(ctx context.Context, d Draft) (*Sent, error) {
	if a.Docks == nil {
		a.Drafts.Put(d)
		return nil, fmt.Errorf("not moved: this assistant has no dock schedule")
	}
	appt, err := a.Docks.MoveDock(ctx, d.Dock.AppointmentID, d.Dock.WasStart, backend.DockSlot{Dock: d.Dock.Dock, Start: d.Dock.Start, End: d.Dock.End})
	if err != nil {
		return nil, a.notSent(d, err)
	}
	a.learned(appt)
	return &Sent{Draft: d, Docked: appt}, nil
}

// learned keeps what the model learned of an appointment (its day, its
// window, its ID) as private as the business file's slot: no question or
// order may carry it.
func (a *Assistant) learned(appt *backend.Appointment) {
	start, end := appt.Start, appt.End.In(appt.Start.Location())
	a.seen.add(appt.ID, start.Format(time.DateOnly), start.Format("15:04")+"-"+end.Format("15:04"))
}
