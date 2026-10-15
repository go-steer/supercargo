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
	"os"
	"path/filepath"
	"testing"

	"github.com/go-steer/supercargo/internal/config"
)

// TestShipperAssistantOrders: the company's own assistant reads its
// business context and its address book's places; the tools an outside
// agent gets over MCP have neither.
func TestShipperAssistantOrders(t *testing.T) {
	c := cfg(t, config.RoleShipper)
	c.LLM = &config.LLM{Provider: config.ProviderFake}
	dir := t.TempDir()
	book := filepath.Join(dir, "book.yaml")
	_ = os.WriteFile(book, []byte(`places:
  - id: tilburg
    name: Tilburg warehouse
    party: { name: Cymbal Shoes B.V. }
    place: { type: door, un_location_code: NLTLB, address: { street: Ringbaan Oost 100, locality: Tilburg, country: NL } }
`), 0o600)
	biz := filepath.Join(dir, "business.yaml")
	_ = os.WriteFile(biz, []byte("shipments:\n  - { shipperRef: CY-PO-1, requiredBy: 2026-11-07 }\n"), 0o600)
	c.AddressBook, c.Business = &config.AddressBook{File: book}, &config.Business{File: biz}
	a, err := ShipperAssistant(c, nil)
	if err != nil || len(a.Places) != 1 || a.Business == nil || len(a.Business.Shipments) != 1 {
		t.Fatalf("own assistant: %+v %v", a, err)
	}
	ext, err := ToolAssistant(c, nil)
	if err != nil || ext.Places != nil || ext.Business != nil || len(ext.Private) == 0 {
		t.Errorf("outside agents' tools: places %v, business %v, private %v, %v", ext.Places, ext.Business, ext.Private, err)
	}
	// A broken address book stops it.
	_ = os.WriteFile(book, []byte("places: [{ id: Bad Id }]\n"), 0o600)
	if _, err := ShipperAssistant(c, nil); err == nil {
		t.Error("a broken address book loaded")
	}
}
