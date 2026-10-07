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
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/supercargo/internal/control"
	"github.com/go-steer/supercargo/internal/shipper"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/intents"
)

// controlNode serves h on a Unix socket, as the node's control API is,
// and returns a ControlNode for it.
func controlNode(t *testing.T, h http.Handler) ControlNode {
	t.Helper()
	dir, err := os.MkdirTemp("", "asst") // short: socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "n.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &httptest.Server{Listener: l, Config: &http.Server{Handler: h}} //nolint:gosec // a test server
	srv.Start()
	t.Cleanup(srv.Close)
	return ControlNode{Client: control.Client(sock, 0)}
}

func answer(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func TestControlNode(t *testing.T) {
	mux := http.NewServeMux()
	var booked shipper.BookRequest
	var asked shipper.AskRequest
	mux.HandleFunc("GET /profile/{domain}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("domain") != "ocean-blue.example" {
			answer(w, http.StatusBadGateway, shipper.ProfileResponse{Error: "no such host"})
			return
		}
		answer(w, http.StatusOK, shipper.ProfileResponse{Profile: &shipper.Profile{Domain: "ocean-blue.example", Name: "Ocean Blue"}})
	})
	mux.HandleFunc("GET /shipments", func(w http.ResponseWriter, _ *http.Request) {
		answer(w, http.StatusOK, shipper.ShipmentsResponse{Shipments: []store.Summary{{Ref: "OB-BKG-1"}}})
	})
	mux.HandleFunc("GET /shipments/{op}/{ref}/events", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("ref") != "OB-BKG-1" {
			answer(w, http.StatusNotFound, shipper.RecordsResponse{Error: "no booking"})
			return
		}
		answer(w, http.StatusOK, shipper.RecordsResponse{Summary: &store.Summary{Ref: "OB-BKG-1", Operator: r.PathValue("op")}})
	})
	mux.HandleFunc("POST /ask", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&asked)
		if asked.Question == "leak" {
			answer(w, http.StatusUnprocessableEntity, shipper.AskResponse{Error: "not sent: private"})
			return
		}
		answer(w, http.StatusOK, shipper.AskResponse{Operator: asked.Operator, Answer: &intents.TnTInquiryResponseBody{Answer: "Soon."}})
	})
	mux.HandleFunc("POST /book", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&booked)
		answer(w, http.StatusOK, shipper.BookResponse{Confirmation: &intents.BookingConfirmationBody{BookingRef: "OB-BKG-2"}})
	})
	mux.HandleFunc("GET /garbled", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) })
	n := controlNode(t, mux)
	ctx := context.Background()

	if p, err := n.Profile(ctx, "ocean-blue.example"); err != nil || p.Name != "Ocean Blue" {
		t.Errorf("Profile: %+v, %v", p, err)
	}
	var ne *NodeError
	if _, err := n.Profile(ctx, "mallory.example"); !errors.As(err, &ne) || ne.Status != http.StatusBadGateway || err.Error() != "no such host" {
		t.Errorf("Profile failure: %v", err)
	}
	if s, err := n.Shipments(ctx); err != nil || len(s) != 1 {
		t.Errorf("Shipments: %+v, %v", s, err)
	}
	if r, err := n.Records(ctx, "ocean-blue.example", "OB-BKG-1"); err != nil || r.Summary.Operator != "ocean-blue.example" {
		t.Errorf("Records: %+v, %v", r, err)
	}
	if _, err := n.Records(ctx, "ocean-blue.example", "OB-BKG-9"); err == nil || err.Error() != "no booking" {
		t.Errorf("Records failure: %v", err)
	}
	if a, err := n.Ask(ctx, "ocean-blue.example", "OB-BKG-1", "When?"); err != nil || a.Answer.Answer != "Soon." || asked.BookingRef != "OB-BKG-1" {
		t.Errorf("Ask: %+v, %v", a, err)
	}
	if _, err := n.Ask(ctx, "", "OB-BKG-1", "leak"); err == nil || !strings.Contains(err.Error(), "private") {
		t.Errorf("Ask refusal: %v", err)
	}
	if b, err := n.Book(ctx, shipper.BookRequest{Operator: "ocean-blue.example"}); err != nil || b.Confirmation.BookingRef != "OB-BKG-2" ||
		booked.Operator != "ocean-blue.example" {
		t.Errorf("Book: %+v, %v", b, err)
	}
	var v struct{}
	if err := n.do(ctx, http.MethodGet, "/garbled", nil, &v); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Errorf("garbled answer: %v", err)
	}
	if err := n.do(ctx, http.MethodGet, "/missing", nil, &v); err == nil {
		t.Error("a 404 with no JSON passed")
	}
	if err := (&NodeError{Status: 500}).Error(); err != "the node answered HTTP 500" {
		t.Errorf("bare NodeError: %s", err)
	}
	down := ControlNode{Client: control.Client(filepath.Join(t.TempDir(), "none.sock"), 0)}
	if _, err := down.Shipments(ctx); err == nil {
		t.Error("no node, no error")
	}
	if err := n.do(ctx, http.MethodPost, "/ask", make(chan int), &v); err == nil {
		t.Error("an unencodable body passed")
	}
}
