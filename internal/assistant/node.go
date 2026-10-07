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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/go-steer/supercargo/internal/shipper"
	"github.com/go-steer/supercargo/internal/store"
)

// Node is what the assistant uses of the shipper's node: its control API
// (internal/shipper's ControlHandler). Book is not behind any tool: only
// the confirmation dialog calls it, after a person says yes.
type Node interface {
	Profile(ctx context.Context, domain string) (*shipper.Profile, error)
	Shipments(ctx context.Context) ([]store.Summary, error)
	Records(ctx context.Context, operator, ref string) (*shipper.RecordsResponse, error)
	Ask(ctx context.Context, operator, ref, question string) (*shipper.AskResponse, error)
	Status(ctx context.Context, operator, ref string) (*shipper.StatusResponse, error)
	Book(ctx context.Context, req shipper.BookRequest) (*shipper.BookResponse, error)
}

// ControlNode is a Node over the control socket's HTTP client
// (control.Client).
type ControlNode struct {
	Client *http.Client
}

// maxAnswer bounds a control API answer.
const maxAnswer = 1 << 20

func (n ControlNode) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://control"+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := n.Client.Do(req)
	if err != nil {
		return fmt.Errorf("the node: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAnswer)).Decode(out); err != nil {
		return fmt.Errorf("the node: HTTP %d, unreadable answer: %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK {
		return &NodeError{Status: resp.StatusCode}
	}
	return nil
}

// NodeError is a control API refusal; the answer's own error field says
// why.
type NodeError struct {
	Status  int
	Message string
}

func (e *NodeError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("the node answered HTTP %d", e.Status)
}

// withMessage fills in a NodeError's message from the answer.
func withMessage(err error, msg string) error {
	var ne *NodeError
	if errors.As(err, &ne) && msg != "" {
		ne.Message = msg
	}
	return err
}

// Profile implements Node.
func (n ControlNode) Profile(ctx context.Context, domain string) (*shipper.Profile, error) {
	var out shipper.ProfileResponse
	if err := n.do(ctx, http.MethodGet, "/profile/"+url.PathEscape(domain), nil, &out); err != nil {
		return nil, withMessage(err, out.Error)
	}
	return out.Profile, nil
}

// Shipments implements Node.
func (n ControlNode) Shipments(ctx context.Context) ([]store.Summary, error) {
	var out shipper.ShipmentsResponse
	if err := n.do(ctx, http.MethodGet, "/shipments", nil, &out); err != nil {
		return nil, err
	}
	return out.Shipments, nil
}

// Records implements Node.
func (n ControlNode) Records(ctx context.Context, operator, ref string) (*shipper.RecordsResponse, error) {
	var out shipper.RecordsResponse
	if err := n.do(ctx, http.MethodGet, "/shipments/"+url.PathEscape(operator)+"/"+url.PathEscape(ref)+"/events", nil, &out); err != nil {
		return nil, withMessage(err, out.Error)
	}
	return &out, nil
}

// Ask implements Node.
func (n ControlNode) Ask(ctx context.Context, operator, ref, question string) (*shipper.AskResponse, error) {
	var out shipper.AskResponse
	err := n.do(ctx, http.MethodPost, "/ask", shipper.AskRequest{Operator: operator, BookingRef: ref, Question: question}, &out)
	if err != nil {
		return nil, withMessage(err, out.Error)
	}
	return &out, nil
}

// Status implements Node.
func (n ControlNode) Status(ctx context.Context, operator, ref string) (*shipper.StatusResponse, error) {
	var out shipper.StatusResponse
	if err := n.do(ctx, http.MethodPost, "/status", shipper.StatusRequest{Operator: operator, BookingRef: ref}, &out); err != nil {
		return nil, withMessage(err, out.Error)
	}
	return &out, nil
}

// Book implements Node.
func (n ControlNode) Book(ctx context.Context, req shipper.BookRequest) (*shipper.BookResponse, error) {
	var out shipper.BookResponse
	if err := n.do(ctx, http.MethodPost, "/book", req, &out); err != nil {
		return nil, withMessage(err, out.Error)
	}
	return &out, nil
}
