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
	"fmt"
	"slices"
	"time"

	"github.com/go-steer/supercargo/internal/store"
)

// Curfew is a city's heavy-truck restriction and the dispatcher's
// standing policy for it (docs/DESIGN.md, "Truck restrictions"): trucks
// may not enter Zone between From and To local time, so a truck that
// would arrive inside the window waits at Yard, a safe staging yard, and
// delivers YardToDelivery after the window ends. The policy is a person's
// (By), set ahead of time; the node applies it as a delay re-plans a leg,
// and says so in the leg's signed alert.
type Curfew struct {
	Zone           string   // e.g. "São Paulo truck restriction (simplified)"
	LOCODEs        []string // deliveries inside it, by UN/LOCODE
	From, To       int      // the window, minutes after local midnight (From < To)
	Loc            *time.Location
	Yard           string
	YardToDelivery time.Duration
	By             string // who set the standing policy
}

// Restrict applies the policy to a truck leg arriving at arrival: when
// its delivery is inside the zone and the arrival inside the window, the
// arrival it plans to instead (the window's end, plus the yard to the
// delivery), and a note for the alert saying so.
func (c *Curfew) Restrict(b store.Booking, arrival time.Time) (time.Time, string, bool) {
	if c == nil || b.Road == nil || !slices.Contains(c.LOCODEs, b.Road.Request.Delivery.UNLocationCode) {
		return arrival, "", false
	}
	local := arrival.In(c.Loc)
	minute := local.Hour()*60 + local.Minute()
	if minute < c.From || minute >= c.To {
		return arrival, "", false
	}
	release := time.Date(local.Year(), local.Month(), local.Day(), c.To/60, c.To%60, 0, 0, c.Loc)
	planned := release.Add(c.YardToDelivery).UTC()
	note := fmt.Sprintf("It would reach the %s at %s local time; by standing policy it waits at %s until %s and delivers about %s.",
		c.Zone, local.Format("15:04"), c.Yard, release.Format("15:04"), planned.In(c.Loc).Format("15:04"))
	return planned, note, true
}
