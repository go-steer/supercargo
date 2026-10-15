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
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/go-steer/supercargo/internal/forwarder"
	"github.com/go-steer/supercargo/internal/llm"
	"github.com/go-steer/supercargo/internal/shipper"
	"github.com/go-steer/supercargo/pkg/intents"
)

// TestControlDesk: orders, proposals and an order's send over the control
// API, with the node's refusals carried as NodeErrors.
func TestControlDesk(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /orders/{ref}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("ref") != "MF-0007" {
			answer(w, http.StatusNotFound, forwarder.OrderResponse{Error: "no order"})
			return
		}
		answer(w, http.StatusOK, forwarder.OrderResponse{Order: &forwarder.OrderView{Ref: "MF-0007"}})
	})
	mux.HandleFunc("POST /proposals/{id}/confirm", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "MF-PR-000008" {
			answer(w, http.StatusConflict, forwarder.ProposalsResponse{Error: "closed"})
			return
		}
		answer(w, http.StatusOK, forwarder.ProposalsResponse{Proposals: []forwarder.Proposal{{ID: "MF-PR-000008", Status: forwarder.ProposalConfirmed}}})
	})
	mux.HandleFunc("POST /orders", func(w http.ResponseWriter, r *http.Request) {
		answer(w, http.StatusOK, shipper.OrderResponse{Answer: &intents.ForwardingConfirmationBody{OrderRef: "MF-0008", Status: intents.StatusPending}})
	})
	n := controlNode(t, mux)
	c := ControlCarrierNode(n)
	ctx := context.Background()
	if v, err := c.Order(ctx, "MF-0007"); err != nil || v.Ref != "MF-0007" {
		t.Errorf("order: %+v %v", v, err)
	}
	var ne *NodeError
	if _, err := c.Order(ctx, "MF-9999"); !errors.As(err, &ne) || ne.Status != http.StatusNotFound || !strings.Contains(err.Error(), "no order") {
		t.Errorf("no order: %v", err)
	}
	if r, err := c.ConfirmProposal(ctx, "MF-PR-000008"); err != nil || r.Proposals[0].Status != forwarder.ProposalConfirmed {
		t.Errorf("confirm: %+v %v", r, err)
	}
	if _, err := c.ConfirmProposal(ctx, "MF-PR-000003"); !errors.As(err, &ne) || ne.Status != http.StatusConflict {
		t.Errorf("closed: %v", err)
	}
	if r, err := n.Order(ctx, shipper.OrderRequest{Forwarder: "meridian-forwarding.example"}); err != nil || r.Answer.OrderRef != "MF-0008" {
		t.Errorf("send an order: %+v %v", r, err)
	}
}

// TestFakeReports: the fake models' reports of a confirmed proposal and
// an order's answer, every outcome.
func TestFakeReports(t *testing.T) {
	for result, want := range map[string]string{
		`{"proposal":"MF-PR-1","order":"MF-0007","status":"confirmed"}`: "took the new plan",
		`{"proposal":"MF-PR-1","order":"MF-0007","status":"refused"}`:   "kept its plan",
		`{"proposal":"MF-PR-1","order":"MF-0007","status":"failed"}`:    "is failed",
		`not json`: "couldn't read",
	} {
		if got := proposalReport(result).Text; !strings.Contains(got, want) {
			t.Errorf("%s: %q", result, got)
		}
	}
	for result, want := range map[string]string{
		`{"forwarder":"m.example","answer":{"forwarder_reference":"MF-0007","status":"pending"}}`:  "planning the legs",
		`{"forwarder":"m.example","answer":{"forwarder_reference":"MF-0007","status":"rejected"}}`: "answered MF-0007: rejected",
		`{}`: "couldn't read",
	} {
		if got := orderReportReply(result).Text; !strings.Contains(got, want) {
			t.Errorf("%s: %q", result, got)
		}
	}
	// No open proposal: the advice without options.
	d := &desk{view: delayedOrder()}
	d.view.Proposals = nil
	a := deskAssistant(d)
	r, err := a.Turn(context.Background(), &Conversation{}, "What should we do about MF-0007?")
	if err != nil || strings.Contains(r.Text, "Options") || len(r.Drafts) != 0 {
		t.Errorf("no proposal: %q %+v %v", r.Text, r.Drafts, err)
	}
	// list_orders through the carrier's dispatcher.
	if _, err := a.runCarrier(context.Background(), llm.ToolCall{Name: ToolDraftProposal, Input: []byte(`{"orderRef":"MF-0007","proposalId":"x"}`)}); err == nil {
		t.Error("drafted an unknown proposal")
	}
}
