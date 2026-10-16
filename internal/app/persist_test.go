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

package app

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-steer/supercargo/internal/config"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/intents"
)

// TestPersistence: a node with a persistence file finds its records and
// its replay window there after a restart; one whose file can't be opened
// or read doesn't start.
func TestPersistence(t *testing.T) {
	file := filepath.Join(t.TempDir(), "node.db")
	c := cfg(t, config.RoleOperator)
	c.Persistence = &config.Persistence{File: file}
	a, err := New(c, quiet())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Carrier.Bookings.Add(store.Booking{Ref: "OB-1042", Operator: "cymbal-shoes.example", Shipper: "north.example"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Carrier.Events.Add("cymbal-shoes.example", "OB-1042", store.Record{ID: "EV-1", Intent: intents.TnTEvent}); err != nil {
		t.Fatal(err)
	}
	if err := a.Replay.Record("north.example", "m-1"); err != nil {
		t.Fatal(err)
	}
	_ = a.DB.Close()

	b, err := New(c, quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.DB.Close() }()
	if got, err := b.Carrier.Bookings.Get("cymbal-shoes.example", "OB-1042"); err != nil || got.Shipper != "north.example" {
		t.Errorf("booking after a restart: %+v, %v", got, err)
	}
	if len(b.Carrier.Events.List("cymbal-shoes.example", "OB-1042")) != 1 {
		t.Error("events after a restart")
	}
	if err := b.Replay.Record("north.example", "m-1"); !errors.Is(err, store.ErrReplay) {
		t.Errorf("a message seen before the restart: %v", err)
	}

	// A node that fails to start after opening its file lets it go.
	broken := cfg(t, config.RoleShipper)
	broken.Persistence = &config.Persistence{File: filepath.Join(t.TempDir(), "cy.db")}
	broken.Business = &config.Business{File: filepath.Join(t.TempDir(), "missing.yaml")}
	if _, err := New(broken, quiet()); err == nil {
		t.Fatal("started without its business file")
	}
	broken.Business = nil
	if c, err := New(broken, quiet()); err != nil {
		t.Errorf("the file is still held by the node that failed: %v", err)
	} else {
		_ = c.DB.Close()
	}

	bad := cfg(t, config.RoleOperator)
	bad.Persistence = &config.Persistence{File: filepath.Join(t.TempDir(), "missing", "node.db")}
	if _, err := New(bad, quiet()); err == nil {
		t.Error("started on a file that can't be opened")
	}
	junk := filepath.Join(t.TempDir(), "junk.db")
	if err := os.WriteFile(junk, []byte("not a database, and long enough to look like a header to anyone reading it ......................................"), 0o600); err != nil {
		t.Fatal(err)
	}
	bad.Persistence.File = junk
	if _, err := New(bad, quiet()); err == nil {
		t.Error("started on a file that isn't a database")
	}
}
