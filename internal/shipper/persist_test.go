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
	"path/filepath"
	"testing"

	"github.com/go-steer/supercargo/internal/persist"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
)

// TestPersistSent: an order whose first answer was lost is still kept
// after a restart, so a status answer can take it up; once taken up it is
// forgotten there too. An order the file can't keep isn't sent.
func TestPersistSent(t *testing.T) {
	ctx := context.Background()
	d, err := persist.Open(filepath.Join(t.TempDir(), "cy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	pending := intents.ForwardingConfirmationBody{OrderRef: "MF-0007", Status: intents.StatusPending, SubscriptionID: "SUB-MF-0007"}
	var sentID string
	s := shipperWith(t, func(h envelope.Header) (string, any) {
		if h.Intent == intents.ForwardingRequest {
			sentID = h.MessageID
		}
		return intents.ForwardingConfirmation, &intents.ForwardingConfirmationBody{} // lost: malformed
	})
	if err := s.PersistSent(d.Bucket("sent")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Order(ctx, meridian, anOrder()); err == nil {
		t.Fatal("a malformed answer taken")
	}

	again := shipperWith(t, statusAnswerFor(&sentID, pending))
	if err := again.PersistSent(d.Bucket("sent")); err != nil {
		t.Fatal(err)
	}
	if _, kept := again.sentTo(sentID, meridian); !kept {
		t.Fatal("the order sent before the restart is forgotten")
	}
	if st, err := again.OrderStatus(ctx, meridian, "CY-PO-55120"); err != nil || !st.Adopted {
		t.Fatalf("taken up after a restart: %+v, %v", st, err)
	}
	third := shipperWith(t, statusAnswerFor(&sentID, pending))
	_ = third.PersistSent(d.Bucket("sent"))
	if len(third.sent) != 0 {
		t.Error("an order taken up is still kept")
	}

	sends := 0
	refused := shipperWith(t, func(envelope.Header) (string, any) {
		sends++
		return intents.ForwardingConfirmation, &pending
	})
	_ = refused.PersistSent(failingSent{})
	if _, err := refused.Order(ctx, meridian, anOrder()); err == nil || sends != 0 {
		t.Errorf("sent without keeping it: %v, %d sent", err, sends)
	}
	_ = d.Bucket("bad").Put("x", "not an order")
	if err := again.PersistSent(d.Bucket("bad")); err == nil {
		t.Error("loaded an order that isn't one")
	}
}

// statusAnswerFor answers a status request naming the order sent in *id.
func statusAnswerFor(id *string, answer intents.ForwardingConfirmationBody) func(envelope.Header) (string, any) {
	return func(h envelope.Header) (string, any) {
		return statusAnswer(found(*id, answer))(h)
	}
}

// failingSent refuses every write.
type failingSent struct{}

func (failingSent) Put(string, any) error                 { return errors.New("disk full") }
func (failingSent) Delete(string) error                   { return errors.New("disk full") }
func (failingSent) DeleteBefore(string) error             { return errors.New("disk full") }
func (failingSent) Load(func(string, []byte) error) error { return nil }
