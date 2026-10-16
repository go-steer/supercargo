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
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/go-steer/supercargo/internal/backend"
)

// The demo's systems behind a Brazilian road carrier's node: a risk
// manager and a toll voucher issuer, in memory. They answer as the real
// ones would in shape (a monitoring protocol, a Mercosul plate, a voucher
// number), labelled with Source; nothing leaves the process.

// MockRisk is a risk manager that clears every leg, except one whose
// cargo description contains Refuse (to show a refusal), under a
// protocol numbered from the booking reference.
type MockRisk struct {
	Source string
	Refuse string
}

// Clear implements backend.RiskClearances.
func (m *MockRisk) Clear(_ context.Context, r backend.RiskRequest) (*backend.Clearance, error) {
	h := hash(r.BookingRef)
	c := &backend.Clearance{Approved: true, Source: m.Source,
		Protocol: fmt.Sprintf("SM-%s-%06d", r.Pickup.Format("2006"), h%1_000_000),
		Vehicle:  plate(h),
		Driver:   fmt.Sprintf("driver %c. %s (CPF ***.%03d.%03d-**)", "ACJLMPRS"[h%8], surnames[h%8], h/7%1000, h/11%1000)}
	if m.Refuse != "" && r.Body != nil && strings.Contains(strings.ToLower(r.Body.Cargo.Description), strings.ToLower(m.Refuse)) {
		c.Approved, c.Vehicle, c.Driver = false, "", ""
	}
	return c, nil
}

// MockVouchers issues a toll voucher numbered from the booking reference.
type MockVouchers struct {
	Source string
}

// Issue implements backend.TollVouchers.
func (m *MockVouchers) Issue(_ context.Context, r backend.VoucherRequest) (*backend.Voucher, error) {
	return &backend.Voucher{Number: fmt.Sprintf("VP-%010d", uint64(hash("vp/"+r.BookingRef))*7%10_000_000_000), Source: m.Source}, nil
}

var surnames = []string{"Silva", "Santos", "Oliveira", "Souza", "Lima", "Pereira", "Costa", "Rodrigues"}

func hash(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}

// plate is a Mercosul plate (LLLNLNN) from h.
func plate(h uint32) string {
	l := func(n uint32) byte { return byte('A' + n%26) }
	return fmt.Sprintf("%c%c%c%d%c%02d", l(h), l(h/26), l(h/676), h/17576%10, l(h/175760), h/4569760%100)
}
