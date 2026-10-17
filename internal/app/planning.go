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
	"fmt"
	"os"
	"time"

	"github.com/go-steer/supercargo/internal/config"
	"github.com/go-steer/supercargo/internal/geo"
	"github.com/go-steer/supercargo/internal/operator"
	"github.com/go-steer/supercargo/internal/qualp"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/internal/weather"
)

// planning is a road carrier's journey planning, from its config.
// Credentials come from the environment, never the file.
func planning(p *config.Planning) (*operator.Planning, error) {
	out := &operator.Planning{}
	if r := p.Route; r != nil {
		token := os.Getenv(r.QualP.TokenEnv)
		if token == "" {
			return nil, fmt.Errorf("road: planning: qualp: $%s is empty", r.QualP.TokenEnv)
		}
		label := r.QualP.Label
		if label == "" {
			label = "QualP"
		}
		out.Route = &qualp.Client{BaseURL: r.QualP.URL, Token: token, Source: label}
		out.Axles, out.LoadType, out.Table = r.Axles, r.LoadType, r.Table
	}
	for _, w := range p.Via {
		pt, _ := geo.Decode(w.PlusCode) // config checked it
		out.Via = append(out.Via, operator.Waypoint{Name: w.Name, Point: pt})
	}
	if w := p.Weather; w != nil {
		if w.Mock != "" {
			out.Weather = &weather.Mock{Source: w.Mock}
		} else {
			key := os.Getenv(w.Google.APIKeyEnv)
			if key == "" {
				return nil, fmt.Errorf("road: planning: weather: $%s is empty", w.Google.APIKeyEnv)
			}
			out.Weather = &weather.Google{Key: key, Language: w.Google.Language}
		}
	}
	return out, nil
}

// curfew applies a city's truck restriction to the carrier's legs on the
// simulator: a leg it re-plans gets the policy's step, and the driver's,
// among what the carrier's systems did.
func (a *App) curfew(c *config.Curfew, driverApp *config.BackOfficeMock) {
	from, to, loc := c.Window()
	cf := &operator.Curfew{Zone: c.Zone, LOCODEs: c.LOCODEs, From: from, To: to, Loc: loc,
		Yard: c.Yard, YardToDelivery: c.YardToDelivery, By: c.By}
	a.Simulator.Restrict = cf.Restrict
	a.Simulator.OnRestricted = func(b store.Booking, note string, until time.Time) {
		now := time.Now()
		release := until.Add(-c.YardToDelivery).In(loc).Format("15:04")
		steps := []store.BackOfficeStep{{System: "policy", Source: c.By, Action: "Standing policy applied: the truck stages at " + c.Yard,
			Reference: note, At: now}}
		if driverApp != nil {
			steps = append(steps, store.BackOfficeStep{System: "driver", Source: driverApp.Mock,
				Action: fmt.Sprintf("Driver told: go to %s, leave at %s", c.Yard, release), At: now})
		}
		_, err := a.Carrier.Bookings.Update(b.Operator, b.Ref, func(sb *store.Booking) error {
			if sb.Road != nil && len(sb.Road.BackOffice)+len(steps) <= maxBackOffice {
				sb.Road.BackOffice = append(sb.Road.BackOffice, steps...)
			}
			return nil
		})
		if err != nil {
			a.Logger.Warn("curfew", "booking", b.Ref, "err", err)
			return
		}
		a.Logger.Info("curfew", "booking", b.Ref, "yard", c.Yard, "until", release, "policy", c.By)
	}
}

// maxBackOffice bounds the steps a leg keeps.
const maxBackOffice = 64
