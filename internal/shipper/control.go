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
	"errors"
	"net/http"
	"strings"

	"github.com/go-steer/supercargo/internal/business"
	"github.com/go-steer/supercargo/internal/control"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/discovery"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
	"github.com/go-steer/supercargo/pkg/transport/rest"
)

// BookRequest is the body of the control API's POST /book: a booking the
// person at the CLI has already confirmed.
type BookRequest struct {
	Operator string                     `json:"operator"`
	Request  intents.BookingRequestBody `json:"request"`
}

// BookResponse is its answer: the operator's confirmation, or why there is
// none.
type BookResponse struct {
	Confirmation *intents.BookingConfirmationBody `json:"confirmation,omitempty"`
	Error        string                           `json:"error,omitempty"`
}

// ControlHandler is the shipper's control API (internal/control). Booking
// goes through the running node so the node records the booking, and so
// accepts the operator's pushes for it.
//
// It sends whatever it is given: the human confirmation the hard rules
// require happens in the CLI before the call, and reaching the socket at
// all takes the node user's own access.
//
// The reads (shipments, a booking's records, a partner's profile) are what
// the TUI and the browser UI show; they serve both alike.
func ControlHandler(s *Shipper, profiles rest.Profiles) http.Handler {
	mux := http.NewServeMux()
	RegisterControl(mux, s, profiles)
	return mux
}

// RegisterControl adds the routes to mux, which may hold another role's:
// a node with several roles serves one control API.
func RegisterControl(mux *http.ServeMux, s *Shipper, profiles rest.Profiles) {
	mux.HandleFunc("GET /shipments", func(w http.ResponseWriter, _ *http.Request) {
		out := []store.Summary{}
		for _, b := range s.Bookings.List() {
			out = append(out, store.Summarize(b, s.records(b.Operator, b.Ref)))
		}
		control.Answer(w, http.StatusOK, ShipmentsResponse{Shipments: out})
	})
	mux.HandleFunc("GET /shipments/{operator}/{ref}/events", func(w http.ResponseWriter, r *http.Request) {
		op, ref := r.PathValue("operator"), r.PathValue("ref")
		b, err := s.Bookings.Get(op, ref)
		if err != nil {
			control.Answer(w, http.StatusNotFound, RecordsResponse{Error: "no booking " + ref + " with " + op})
			return
		}
		recs := s.records(op, ref) // once: the summary and the records must agree
		control.Answer(w, http.StatusOK, RecordsResponse{Summary: ptr(store.Summarize(b, recs)),
			Records: store.ViewRecords(recs)})
	})
	mux.HandleFunc("GET /profile/{domain}", func(w http.ResponseWriter, r *http.Request) {
		domain := r.PathValue("domain")
		if !envelope.ValidDomain(domain) {
			control.Answer(w, http.StatusBadRequest, ProfileResponse{Error: "not a lowercase domain"})
			return
		}
		// Fetched through the node's key cache: the guarded client, the
		// strict parser, and the same keys the node verifies with.
		doc, err := profiles.Profile(r.Context(), domain)
		if err != nil {
			control.Answer(w, http.StatusBadGateway, ProfileResponse{Error: err.Error()})
			return
		}
		control.Answer(w, http.StatusOK, ProfileResponse{Profile: SummarizeProfile(doc)})
	})
	mux.HandleFunc("POST /book", func(w http.ResponseWriter, r *http.Request) {
		var req BookRequest
		if err := control.Decode(r, &req); err != nil {
			control.Answer(w, http.StatusBadRequest, BookResponse{Error: err.Error()})
			return
		}
		if !envelope.ValidDomain(req.Operator) || req.Operator == s.Node.Domain() {
			control.Answer(w, http.StatusBadRequest, BookResponse{Error: "operator must be another party's lowercase domain"})
			return
		}
		if err := req.Request.Validate(); err != nil {
			control.Answer(w, http.StatusBadRequest, BookResponse{Error: "request: " + err.Error()})
			return
		}
		conf, err := s.Book(r.Context(), req.Operator, &req.Request)
		if errors.Is(err, ErrNoShipperRole) {
			control.Answer(w, http.StatusForbidden, BookResponse{Error: err.Error()})
			return
		}
		if errors.Is(err, business.ErrLeak) {
			control.Answer(w, http.StatusUnprocessableEntity, BookResponse{Error: leakMessage})
			return
		}
		if err != nil {
			control.Answer(w, http.StatusBadGateway, BookResponse{Error: err.Error()})
			return
		}
		control.Answer(w, http.StatusOK, BookResponse{Confirmation: conf})
	})
	// operatorOf finds the operator of ref when the request names none;
	// it answers and reports false when it can't.
	operatorOf := func(op, ref string, fail func(int, string)) (string, bool) {
		if op != "" {
			return op, true
		}
		// The reference alone names it if exactly one operator has it.
		switch ops := s.Bookings.Operators(ref); len(ops) {
		case 0:
			fail(http.StatusNotFound, "no booking "+ref)
			return "", false
		case 1:
			return ops[0], true
		default:
			fail(http.StatusConflict, "several operators have a booking "+ref+"; name one: "+strings.Join(ops, ", "))
			return "", false
		}
	}
	mux.HandleFunc("POST /ask", func(w http.ResponseWriter, r *http.Request) {
		var req AskRequest
		if err := control.Decode(r, &req); err != nil {
			control.Answer(w, http.StatusBadRequest, AskResponse{Error: err.Error()})
			return
		}
		op, ok := operatorOf(req.Operator, req.BookingRef, func(code int, msg string) { control.Answer(w, code, AskResponse{Error: msg}) })
		if !ok {
			return
		}
		a, err := s.Ask(r.Context(), op, req.BookingRef, req.Question)
		switch {
		case errors.Is(err, ErrNotOurBooking):
			control.Answer(w, http.StatusNotFound, AskResponse{Error: err.Error()})
		case errors.Is(err, business.ErrLeak):
			control.Answer(w, http.StatusUnprocessableEntity, AskResponse{Error: leakMessage})
		case err != nil:
			control.Answer(w, http.StatusBadGateway, AskResponse{Error: err.Error()})
		default:
			control.Answer(w, http.StatusOK, AskResponse{Operator: a.Operator, Answer: a.Response, Held: a.Held})
		}
	})
	mux.HandleFunc("POST /status", func(w http.ResponseWriter, r *http.Request) {
		var req StatusRequest
		if err := control.Decode(r, &req); err != nil {
			control.Answer(w, http.StatusBadRequest, StatusResponse{Error: err.Error()})
			return
		}
		op, ok := operatorOf(req.Operator, req.BookingRef, func(code int, msg string) { control.Answer(w, code, StatusResponse{Error: msg}) })
		if !ok {
			return
		}
		st, err := s.Status(r.Context(), op, req.BookingRef)
		switch {
		case errors.Is(err, ErrNotOurBooking):
			control.Answer(w, http.StatusNotFound, StatusResponse{Error: err.Error()})
		case err != nil:
			control.Answer(w, http.StatusBadGateway, StatusResponse{Error: err.Error()})
		default:
			control.Answer(w, http.StatusOK, StatusResponse{Operator: st.Operator, Status: st.Status, Missing: st.Missing})
		}
	})
}

// StatusRequest is the body of the control API's POST /status: ask a
// booking's operator for its DCSA status. Operator may be empty when only
// one operator has a booking under BookingRef.
type StatusRequest struct {
	Operator   string `json:"operator,omitempty"`
	BookingRef string `json:"bookingRef"`
}

// StatusResponse is its answer: the operator's signed report, and the
// reported events this shipper never received as pushes.
type StatusResponse struct {
	Operator string             `json:"operator,omitempty"`
	Status   *intents.TnTStatus `json:"status,omitempty"`
	Missing  []string           `json:"missing,omitempty"`
	Error    string             `json:"error,omitempty"`
}

// AskRequest is the body of the control API's POST /ask. Operator may be
// empty when only one operator has a booking under BookingRef.
type AskRequest struct {
	Operator   string `json:"operator,omitempty"`
	BookingRef string `json:"bookingRef"`
	Question   string `json:"question"`
}

// AskResponse is its answer.
type AskResponse struct {
	Operator string                          `json:"operator,omitempty"`
	Answer   *intents.TnTInquiryResponseBody `json:"answer,omitempty"`
	Held     []string                        `json:"held,omitempty"`
	Error    string                          `json:"error,omitempty"`
}

// ShipmentsResponse is GET /shipments.
type ShipmentsResponse struct {
	Shipments []store.Summary `json:"shipments"`
}

// RecordsResponse is GET /shipments/{operator}/{ref}/events: the booking
// at a glance and its records, oldest first.
type RecordsResponse struct {
	Summary *store.Summary     `json:"summary,omitempty"`
	Records []store.RecordView `json:"records,omitempty"`
	Error   string             `json:"error,omitempty"`
}

// ProfileResponse is GET /profile/{domain}.
type ProfileResponse struct {
	Profile *Profile `json:"profile,omitempty"`
	Error   string   `json:"error,omitempty"`
}

// Profile is a partner's verified profile at a glance.
type Profile struct {
	Domain  string   `json:"domain"`
	Name    string   `json:"name"`
	Roles   []string `json:"roles"`
	Intents []string `json:"intents"`
	REST    string   `json:"rest"`
	A2A     string   `json:"a2a,omitempty"`
	MCP     string   `json:"mcp,omitempty"`
	Keys    []string `json:"keys"` // kids
	// Capabilities are the profile's capability names with their tags
	// ("dev.ulp.logistics.tracking (ulp:service, ulp:transport:rest)").
	Capabilities []string `json:"capabilities,omitempty"`
	Dialects     []string `json:"supportedDialects,omitempty"`
}

// The summary's caps: a partner writes its profile, and the summary reaches
// the assistant's model, so it carries a bounded amount of that text.
const (
	summaryIntents      = 32
	summaryCapabilities = 8
	summaryTags         = 8
)

// SummarizeProfile sums up a parsed, validated profile, within the caps.
func SummarizeProfile(doc *discovery.Document) *Profile {
	p := doc.Profile
	out := &Profile{Domain: p.Party.Domain, Name: p.Party.Name, Roles: p.Party.Roles, REST: p.Transports.REST,
		A2A: p.Transports.A2A, MCP: p.Transports.MCP, Intents: []string{}, Keys: doc.KIDs(), Dialects: p.SupportedDialects}
	for _, c := range p.Capabilities[:min(len(p.Capabilities), summaryCapabilities)] {
		line := c.Name
		if len(c.Tags) > 0 {
			line += " (" + strings.Join(c.Tags[:min(len(c.Tags), summaryTags)], ", ") + ")"
		}
		out.Capabilities = append(out.Capabilities, line)
	}
	for _, svc := range p.Services {
		out.Intents = append(out.Intents, svc.Intents...)
	}
	out.Intents = out.Intents[:min(len(out.Intents), summaryIntents)]
	return out
}

func (s *Shipper) records(operator, ref string) []store.Record {
	if s.Events == nil {
		return nil
	}
	return s.Events.List(operator, ref)
}

func ptr[T any](v T) *T { return &v }

// leakMessage explains a send refused by the business-context guard: a
// local decision, nothing to do with the partner.
const leakMessage = "not sent: it would carry text from your private business context " +
	"(a shipment note, a warehouse name or a receiving window); reword it and try again"
