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
	"testing"
)

// TestDoorPlusCode: an order's door pinned by a Plus Code reaches the
// truck's stop.
func TestDoorPlusCode(t *testing.T) {
	legs := &carriers{}
	orch, _ := orchestrated(t, legs, portToDoor)
	o := doorOrder()
	o.PlaceOfDelivery.PlusCode = "588MGFP8+VM"
	if _, err := orch.Orders.Handle(context.Background(), orderFrom(cymbal, o)); err != nil {
		t.Fatal(err)
	}
	if _, err := orch.Book(context.Background(), "MF-0007"); err != nil {
		t.Fatal(err)
	}
	if legs.got.Delivery.PlusCode != "588MGFP8+VM" {
		t.Errorf("the truck's delivery: %+v", legs.got.Delivery)
	}
}
