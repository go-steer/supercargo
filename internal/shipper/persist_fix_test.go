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
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/persist"
	"github.com/go-steer/supercargo/pkg/intents"
)

// TestPersistSentOrder: saved orders come back in the order sent, even
// sent at the same moment (the clock doesn't order them), and new ones
// follow; a delete the file refuses is reported.
func TestPersistSentOrder(t *testing.T) {
	d, err := persist.Open(filepath.Join(t.TempDir(), "cy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	at := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	s := shipperWith(t, statusAnswer(intents.ForwardingStatusResponseBody{ShipperRef: "CY-PO-55120"}))
	s.Now = func() time.Time { return at }
	_ = s.PersistSent(d.Bucket("sent"))
	ids := []string{"ffffffff-0000-4000-8000-000000000000", "00000000-0000-4000-8000-000000000000", "88888888-0000-4000-8000-000000000000"}
	for _, id := range ids {
		if err := s.rememberSent(id, meridian, anOrder()); err != nil {
			t.Fatal(err)
		}
	}
	again := shipperWith(t, statusAnswer(intents.ForwardingStatusResponseBody{ShipperRef: "CY-PO-55120"}))
	_ = again.PersistSent(d.Bucket("sent"))
	if !slices.Equal(again.sentOrder, ids) {
		t.Errorf("restored in the order %v, want %v", again.sentOrder, ids)
	}
	if err := again.rememberSent("11111111-0000-4000-8000-000000000000", meridian, anOrder()); err != nil {
		t.Fatal(err)
	}
	if again.sent["11111111-0000-4000-8000-000000000000"].n != 4 {
		t.Errorf("a new order's place: %d", again.sent["11111111-0000-4000-8000-000000000000"].n)
	}
	var reported []error
	again.OnSaveError = func(err error) { reported = append(reported, err) }
	again.sentSaver = failingSent{}
	again.forgetSent(ids[0])
	if len(reported) != 1 {
		t.Errorf("a failed delete reported %d times", len(reported))
	}
}
