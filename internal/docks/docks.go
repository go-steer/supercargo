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

// Package docks is a receiving site's dock schedule as the shipper's node
// reaches it (backend.DockAppointments): the demo's in-memory mock, seeded
// from the business context's receiving slots, and the control API's
// routes. The schedule is the shipper's own: nothing here goes in an
// envelope, and only a person's confirmation moves an appointment.
package docks

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-steer/supercargo/internal/backend"
	"github.com/go-steer/supercargo/internal/business"
	"github.com/go-steer/supercargo/internal/control"
)

// Windows are the mock's slots each day, local time: four hours each.
var Windows = []int{6, 10, 14, 18}

// MaxDays bounds a search for slots.
const MaxDays = 14

var (
	// ErrNoAppointment is a shipment without one.
	ErrNoAppointment = errors.New("docks: no appointment for this shipment")
	// ErrSlotTaken is a slot that isn't free (or isn't one).
	ErrSlotTaken = errors.New("docks: the slot isn't free")
	// ErrMoved is an appointment no longer where the move expects it.
	ErrMoved = errors.New("docks: the appointment has moved since")
)

// Mock is the demo's dock schedule: Docks at Site, slots of four hours
// from Windows in Zone, about a third taken by others (the same ones
// every run), and the shipments' appointments.
type Mock struct {
	Source string
	Site   string
	Docks  []string
	Zone   *time.Location
	Now    func() time.Time

	mu    sync.Mutex
	appts map[string]*backend.Appointment // by ID
}

// Seed books the business context's receiving slots as appointments: a
// slot's date and window, at the first dock (or the one its warehouse
// names).
func (m *Mock) Seed(biz *business.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.appts == nil {
		m.appts = map[string]*backend.Appointment{}
	}
	if biz == nil {
		return nil
	}
	for i, s := range biz.Shipments {
		if s.ReceivingSlot == nil || s.ReceivingSlot.Window == "" {
			continue
		}
		start, end, err := m.window(s.ReceivingSlot.Date, s.ReceivingSlot.Window)
		if err != nil {
			return fmt.Errorf("docks: %s: %w", s.ShipperRef, err)
		}
		dock, named := m.Docks[0], ""
		for _, d := range m.Docks {
			if strings.Contains(strings.ToLower(s.ReceivingSlot.Warehouse), strings.ToLower(d)) && len(d) > len(named) {
				dock, named = d, d // the longest named: "Dock 12" over "Dock 1"
			}
		}
		id := fmt.Sprintf("AG-%06d", 4100+i)
		m.appts[id] = &backend.Appointment{ID: id, ShipperRef: s.ShipperRef, Site: m.Site, Dock: dock, Start: start, End: end, Source: m.Source}
	}
	return nil
}

func (m *Mock) window(date, window string) (time.Time, time.Time, error) {
	from, to, ok := strings.Cut(window, "-")
	if !ok {
		return time.Time{}, time.Time{}, fmt.Errorf("window %q", window)
	}
	start, err := time.ParseInLocation("2006-01-02 15:04", date+" "+from, m.Zone)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	end, err := time.ParseInLocation("2006-01-02 15:04", date+" "+to, m.Zone)
	if err != nil || !end.After(start) {
		return time.Time{}, time.Time{}, fmt.Errorf("window %q", window)
	}
	return start, end, nil
}

// Appointment implements backend.DockAppointments.
func (m *Mock) Appointment(_ context.Context, ref string) (*backend.Appointment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var found *backend.Appointment
	for _, a := range m.appts {
		if a.ShipperRef == ref && (found == nil || a.ID < found.ID) {
			found = a // the first booked, if a reference has two
		}
	}
	if found == nil {
		return nil, fmt.Errorf("%w: %s", ErrNoAppointment, ref)
	}
	c := *found
	return &c, nil
}

// Slots implements backend.DockAppointments: the free slots from from's
// calendar day, for days days, that start after now.
func (m *Mock) Slots(_ context.Context, from time.Time, days int) ([]backend.DockSlot, error) {
	if days < 1 || days > MaxDays {
		return nil, fmt.Errorf("docks: 1-%d days", MaxDays)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.free(from, days, ""), nil
}

// free are the free slots from from's calendar day (in its own offset)
// for days days, not counting appointment except's own.
func (m *Mock) free(from time.Time, days int, except string) []backend.DockSlot {
	now := m.Now()
	// The day as given (its own calendar date), at the site.
	day := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, m.Zone)
	var out []backend.DockSlot
	for d := range days {
		for _, h := range Windows {
			start := time.Date(day.Year(), day.Month(), day.Day()+d, h, 0, 0, 0, m.Zone)
			if !start.After(now) {
				continue
			}
			for _, dock := range m.Docks {
				s := backend.DockSlot{Dock: dock, Start: start, End: start.Add(4 * time.Hour)}
				if !m.taken(s, except) {
					out = append(out, s)
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}

// taken: an appointment holds it, or another company's does (a third,
// the same each run).
func (m *Mock) taken(s backend.DockSlot, except string) bool {
	for _, a := range m.appts {
		if a.ID != except && a.Dock == s.Dock && a.Start.Before(s.End) && s.Start.Before(a.End) {
			return true
		}
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(m.Site + "|" + s.Dock + "|" + s.Start.UTC().Format(time.RFC3339)))
	return h.Sum32()%3 == 0
}

// Move implements backend.DockAppointments: to a free slot, its own.
func (m *Mock) Move(_ context.Context, id string, from time.Time, to backend.DockSlot) (*backend.Appointment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.appts[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoAppointment, id)
	}
	if !a.Start.Equal(from) {
		return nil, fmt.Errorf("%w: %s is at %s", ErrMoved, id, a.Start.Format(time.RFC3339))
	}
	// The slot's day at the site, whatever offset it came in.
	if !slices.ContainsFunc(m.free(to.Start.In(m.Zone), 1, id), func(s backend.DockSlot) bool {
		return s.Dock == to.Dock && s.Start.Equal(to.Start) && s.End.Equal(to.End)
	}) {
		return nil, fmt.Errorf("%w: %s %s", ErrSlotTaken, to.Dock, to.Start.Format(time.RFC3339))
	}
	a.Dock, a.Start, a.End = to.Dock, to.Start, to.End
	c := *a
	return &c, nil
}

// AppointmentResponse answers GET /docks/appointments/{ref} and POST
// /docks/appointments/{id}/move.
type AppointmentResponse struct {
	Appointment *backend.Appointment `json:"appointment,omitempty"`
	Error       string               `json:"error,omitempty"`
}

// SlotsResponse answers GET /docks/slots?from=YYYY-MM-DD&days=N.
type SlotsResponse struct {
	Slots []backend.DockSlot `json:"slots"`
	Error string             `json:"error,omitempty"`
}

// MoveRequest is the body of POST /docks/appointments/{id}/move: the
// slot a person confirmed, and where the appointment was when they did.
type MoveRequest struct {
	From  time.Time `json:"from"`
	Dock  string    `json:"dock"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// RegisterControl adds the dock schedule's routes to the control API
// (internal/control): reads, and the move a person confirmed.
func RegisterControl(mux *http.ServeMux, d backend.DockAppointments) {
	mux.HandleFunc("GET /docks/appointments/{ref}", func(w http.ResponseWriter, r *http.Request) {
		a, err := d.Appointment(r.Context(), r.PathValue("ref"))
		if err != nil {
			control.Answer(w, status(err), AppointmentResponse{Error: err.Error()})
			return
		}
		control.Answer(w, http.StatusOK, AppointmentResponse{Appointment: a})
	})
	mux.HandleFunc("GET /docks/slots", func(w http.ResponseWriter, r *http.Request) {
		from, err := time.Parse(time.DateOnly, r.URL.Query().Get("from"))
		days, derr := strconv.Atoi(r.URL.Query().Get("days"))
		if err != nil || derr != nil {
			control.Answer(w, http.StatusBadRequest, SlotsResponse{Error: "from (YYYY-MM-DD) and days are required"})
			return
		}
		s, err := d.Slots(r.Context(), from, days)
		if err != nil {
			control.Answer(w, http.StatusBadRequest, SlotsResponse{Error: err.Error()})
			return
		}
		if s == nil {
			s = []backend.DockSlot{}
		}
		control.Answer(w, http.StatusOK, SlotsResponse{Slots: s})
	})
	mux.HandleFunc("POST /docks/appointments/{id}/move", func(w http.ResponseWriter, r *http.Request) {
		var req MoveRequest
		if err := control.Decode(r, &req); err != nil {
			control.Answer(w, http.StatusBadRequest, AppointmentResponse{Error: err.Error()})
			return
		}
		if req.Dock == "" || req.From.IsZero() || req.Start.IsZero() || !req.End.After(req.Start) {
			control.Answer(w, http.StatusBadRequest, AppointmentResponse{Error: "from, dock, start and a later end are required"})
			return
		}
		a, err := d.Move(r.Context(), r.PathValue("id"), req.From, backend.DockSlot{Dock: req.Dock, Start: req.Start, End: req.End})
		if err != nil {
			control.Answer(w, status(err), AppointmentResponse{Error: err.Error()})
			return
		}
		control.Answer(w, http.StatusOK, AppointmentResponse{Appointment: a})
	})
}

func status(err error) int {
	switch {
	case errors.Is(err, ErrNoAppointment):
		return http.StatusNotFound
	case errors.Is(err, ErrSlotTaken), errors.Is(err, ErrMoved):
		return http.StatusConflict
	}
	return http.StatusBadGateway
}
