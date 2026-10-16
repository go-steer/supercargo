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
	"fmt"
	"testing"
	"time"
)

// TestFloorCacheBound: a full cache drops expired floors first, then the
// oldest, never growing past its bound.
func TestFloorCacheBound(t *testing.T) {
	ctx := context.Background()
	orch := freightOrder(t, &carriers{}, &floors{}, 1050)
	now := orch.Now()
	orch.floors.m = map[string]floorEntry{}
	for i := range maxFloors {
		orch.floors.m[fmt.Sprint(i)] = floorEntry{at: now.Add(-time.Duration(i) * time.Minute)} // the last ones expired
	}
	_, _ = orch.PlanChecked(ctx, "MF-0007")
	if n := len(orch.floors.m); n >= maxFloors || n < maxFloors-(maxFloors-60) {
		t.Errorf("%d floors kept after expiring", n)
	}
	orch.floors.m = map[string]floorEntry{}
	for i := range maxFloors {
		orch.floors.m[fmt.Sprint("fresh", i)] = floorEntry{at: now.Add(-time.Duration(i) * time.Second)}
	}
	orch.Lanes[0].Legs[1].Freight.Axles = 6
	_, _ = orch.PlanChecked(ctx, "MF-0007")
	_, oldest := orch.floors.m[fmt.Sprint("fresh", maxFloors-1)]
	if n := len(orch.floors.m); n != maxFloors || oldest {
		t.Errorf("%d floors kept (the oldest kept: %v)", n, oldest)
	}
}
