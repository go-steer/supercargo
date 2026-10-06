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
	"errors"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/llm"
	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/pkg/intents"
)

// blockingModel answers only when release is closed (never, if nil), or
// fails when its context ends.
type blockingModel struct {
	release chan struct{}
	started chan struct{}
}

func (*blockingModel) Name() string { return "blocking" }

func (m *blockingModel) Generate(ctx context.Context, _ llm.GenerateRequest) (llm.GenerateResponse, error) {
	if m.started != nil {
		m.started <- struct{}{}
	}
	select {
	case <-m.release:
		return llm.GenerateResponse{Text: `{"answer": "Departed.", "citedEventIds": ["EV-000002"]}`}, nil
	case <-ctx.Done():
		return llm.GenerateResponse{}, ctx.Err()
	}
}

// TestInquiryLimits: one partner can't run more than MaxPerSender
// inquiries at once, nor everyone more than MaxInFlight; over a limit the
// answer is a retryable refusal, and slots come back when inquiries end.
func TestInquiryLimits(t *testing.T) {
	m := &blockingModel{release: make(chan struct{}), started: make(chan struct{}, 10)}
	f := newInquiryFixture(t, m)
	f.svc.MaxPerSender, f.svc.MaxInFlight, f.svc.Timeout = 1, 2, 5*time.Second
	f.svc.OnModelCall = nil // the fixture's recorder isn't safe for concurrent use
	handle := func(sender string) error {
		_, err := f.svc.Handle(context.Background(), inquiryReq(sender, "OB-BKG-1", "Where is it?"))
		return err
	}
	done := make(chan error, 2)
	go func() { done <- handle("cymbal-shoes.example") }()
	<-m.started
	var r *node.Rejection
	if err := handle("cymbal-shoes.example"); !errors.As(err, &r) || r.Code != intents.CodeInternal || !r.Retryable {
		t.Errorf("a second inquiry from the same partner: %v, want a retryable refusal", err)
	}
	go func() { done <- handle("other-shipper.example") }()
	<-m.started
	if err := handle("third.example"); !errors.As(err, &r) || !r.Retryable {
		t.Errorf("a third inquiry in flight: %v, want a retryable refusal", err)
	}
	close(m.release)
	for range 2 {
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
	if err := handle("cymbal-shoes.example"); err != nil {
		t.Errorf("after the others ended: %v", err)
	}
	f.svc.mu.Lock()
	defer f.svc.mu.Unlock()
	if f.svc.inFlight != 0 || len(f.svc.bySender) != 0 {
		t.Errorf("slots leaked: %d in flight, %v", f.svc.inFlight, f.svc.bySender)
	}
}
