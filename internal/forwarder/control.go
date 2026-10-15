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

package forwarder

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-steer/supercargo/internal/control"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/intents"
)

// AnswerRequest is the body of the control API's POST /orders/answer: a
// pending order's final answer, which the person at the forwarder has
// already confirmed.
type AnswerRequest struct {
	OrderRef string                             `json:"orderRef"`
	Answer   intents.ForwardingConfirmationBody `json:"answer"`
}

// PlanResponse is the answer to GET /orders/{ref}/plan.
type PlanResponse struct {
	Steps []Step `json:"steps,omitempty"`
	Error string `json:"error,omitempty"`
}

// BookResponse is the answer to POST /orders/{ref}/book: the answer sent
// to the order, or why there is none (and the plan as far as it got).
type BookResponse struct {
	Answer *intents.ForwardingConfirmationBody `json:"answer,omitempty"`
	Steps  []Step                              `json:"steps,omitempty"`
	Error  string                              `json:"error,omitempty"`
}

// AnswerResponse is its outcome.
type AnswerResponse struct {
	Answer *intents.ForwardingConfirmationBody `json:"answer,omitempty"`
	Error  string                              `json:"error,omitempty"`
}

// ProposalsResponse is the answer to GET /orders/{ref}/proposals and
// POST /proposals/{id}/confirm and /dismiss.
type ProposalsResponse struct {
	Proposals []Proposal `json:"proposals,omitempty"`
	Error     string     `json:"error,omitempty"`
}

// OrderView is one order as the forwarder's desk shows it: what the
// shipper signed, the answer it was sent, the legs booked, the plan on
// its lane, the journey from the order's own records (the legs' events as
// re-issued), and the proposals waiting for a person.
type OrderView struct {
	Ref     string                             `json:"orderRef"`
	Shipper string                             `json:"shipper"`
	Request intents.ForwardingRequestBody      `json:"request"`
	Answer  intents.ForwardingConfirmationBody `json:"answer"`
	Legs    []store.LegRef                     `json:"legs"`
	// Withdrawn are legs booked for the order and then cancelled.
	Withdrawn []store.LegRef `json:"withdrawn,omitempty"`
	Steps     []Step         `json:"steps,omitempty"`
	// PlanError is why there is no plan (no lane serves the order).
	PlanError string `json:"planError,omitempty"`
	// Journey is measured against the plan the shipper was told: a leg
	// re-timed since (a confirmed proposal) still shows as late as the
	// promise it moved off.
	Journey   store.Journey `json:"journey"`
	Proposals []Proposal    `json:"proposals"`
}

// OrderResponse is the answer to GET /orders/{ref}.
type OrderResponse struct {
	Order *OrderView `json:"order,omitempty"`
	Error string     `json:"error,omitempty"`
}

// RegisterControl adds the forwarder's routes to mux (internal/control).
// Like the shipper's POST /book, it sends what it is given: the human
// confirmation happens before the call, and reaching the socket takes the
// node user's own access. The orders themselves are listed with the
// carrier side's bookings (GET /bookings); log holds their events, as
// the carrier side published them.
func RegisterControl(mux *http.ServeMux, o *Orders, orch *Orchestrator, relay *Relay, log *store.EventLog) {
	mux.HandleFunc("GET /orders/{ref}", func(w http.ResponseWriter, r *http.Request) {
		ref := r.PathValue("ref")
		b, err := o.Bookings.Get(o.Node.Domain(), ref)
		if err != nil || b.Order == nil {
			control.Answer(w, http.StatusNotFound, OrderResponse{Error: ErrUnknownOrder.Error() + ": " + ref})
			return
		}
		v := &OrderView{Ref: b.Ref, Shipper: b.Shipper, Request: b.Order.Request, Answer: b.Order.Confirmation,
			Legs: append([]store.LegRef{}, b.Order.Legs...), Withdrawn: append([]store.LegRef(nil), b.Order.Withdrawn...), Proposals: []Proposal{}}
		if orch != nil {
			if v.Steps, err = orch.plan(b); err != nil {
				v.PlanError = err.Error()
			}
		}
		var recs []store.Record
		if log != nil {
			recs = log.List(b.Operator, b.Ref)
		}
		v.Journey = store.BuildJourney(b, recs)
		if relay != nil {
			v.Proposals = relay.Proposals(ref)
		}
		control.Answer(w, http.StatusOK, OrderResponse{Order: v})
	})
	if relay != nil {
		mux.HandleFunc("GET /orders/{ref}/proposals", func(w http.ResponseWriter, r *http.Request) {
			control.Answer(w, http.StatusOK, ProposalsResponse{Proposals: relay.Proposals(r.PathValue("ref"))})
		})
		// The person at the forwarder decided against a proposal.
		mux.HandleFunc("POST /proposals/{id}/dismiss", func(w http.ResponseWriter, r *http.Request) {
			p, err := relay.Dismiss(r.PathValue("id"))
			switch {
			case errors.Is(err, ErrUnknownProposal):
				control.Answer(w, http.StatusNotFound, ProposalsResponse{Error: err.Error()})
			case errors.Is(err, ErrProposalClosed):
				control.Answer(w, http.StatusConflict, ProposalsResponse{Error: err.Error()})
			default:
				control.Answer(w, http.StatusOK, ProposalsResponse{Proposals: []Proposal{p}})
			}
		})
		// The person at the forwarder confirmed a proposal: send its
		// amendment to the leg's carrier.
		mux.HandleFunc("POST /proposals/{id}/confirm", func(w http.ResponseWriter, r *http.Request) {
			p, err := relay.Confirm(context.WithoutCancel(r.Context()), r.PathValue("id"))
			switch {
			case errors.Is(err, ErrUnknownProposal):
				control.Answer(w, http.StatusNotFound, ProposalsResponse{Error: err.Error()})
			case errors.Is(err, ErrProposalClosed):
				control.Answer(w, http.StatusConflict, ProposalsResponse{Error: err.Error()})
			case err != nil:
				control.Answer(w, http.StatusBadGateway, ProposalsResponse{Error: err.Error(), Proposals: []Proposal{p}})
			default:
				control.Answer(w, http.StatusOK, ProposalsResponse{Proposals: []Proposal{p}})
			}
		})
	}
	if orch != nil {
		mux.HandleFunc("GET /orders/{ref}/plan", func(w http.ResponseWriter, r *http.Request) {
			steps, err := orch.Plan(r.PathValue("ref"))
			switch {
			case errors.Is(err, ErrUnknownOrder):
				control.Answer(w, http.StatusNotFound, PlanResponse{Error: err.Error()})
			case err != nil:
				control.Answer(w, http.StatusUnprocessableEntity, PlanResponse{Error: err.Error()})
			default:
				control.Answer(w, http.StatusOK, PlanResponse{Steps: steps})
			}
		})
		// The person at the forwarder confirmed the plan: book its legs
		// and answer the order.
		mux.HandleFunc("POST /orders/{ref}/book", func(w http.ResponseWriter, r *http.Request) {
			ref := r.PathValue("ref")
			// Detached from the request: a client going away mid-leg must
			// not leave a leg confirmed by its carrier but not recorded.
			answer, err := orch.Book(context.WithoutCancel(r.Context()), ref)
			steps, _ := orch.Plan(ref)
			switch {
			case errors.Is(err, ErrUnknownOrder):
				control.Answer(w, http.StatusNotFound, BookResponse{Error: err.Error()})
			case errors.Is(err, ErrAnswered), errors.Is(err, ErrCancelPending):
				control.Answer(w, http.StatusConflict, BookResponse{Error: err.Error(), Steps: steps})
			case errors.Is(err, ErrNoLane), errors.Is(err, ErrEDILeg), errors.Is(err, ErrInvalidAnswer), errors.Is(err, ErrPlanUnfit), errors.Is(err, ErrPlanChanged):
				control.Answer(w, http.StatusUnprocessableEntity, BookResponse{Error: err.Error(), Steps: steps})
			case err != nil:
				control.Answer(w, http.StatusBadGateway, BookResponse{Error: err.Error(), Steps: steps})
			default:
				control.Answer(w, http.StatusOK, BookResponse{Answer: answer, Steps: steps})
			}
		})
	}
	mux.HandleFunc("POST /orders/answer", func(w http.ResponseWriter, r *http.Request) {
		var req AnswerRequest
		if err := control.Decode(r, &req); err != nil {
			control.Answer(w, http.StatusBadRequest, AnswerResponse{Error: err.Error()})
			return
		}
		a := req.Answer
		var err error
		if a.Status == intents.StatusRejected && orch != nil {
			// Its legs booked so far are cancelled first.
			err = orch.Reject(context.WithoutCancel(r.Context()), req.OrderRef, &a)
		} else {
			err = o.Answer(context.WithoutCancel(r.Context()), req.OrderRef, &a)
		}
		switch {
		case errors.Is(err, ErrUnknownOrder):
			control.Answer(w, http.StatusNotFound, AnswerResponse{Error: err.Error()})
		case errors.Is(err, ErrAnswered), errors.Is(err, ErrLegsBooked), errors.Is(err, ErrLegKept), errors.Is(err, ErrCancelPending):
			control.Answer(w, http.StatusConflict, AnswerResponse{Error: err.Error()})
		case errors.Is(err, ErrInvalidAnswer):
			control.Answer(w, http.StatusBadRequest, AnswerResponse{Error: err.Error()})
		case err != nil:
			control.Answer(w, http.StatusBadGateway, AnswerResponse{Error: err.Error()})
		default:
			control.Answer(w, http.StatusOK, AnswerResponse{Answer: &a})
		}
	})
}
