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

package config

import (
	"strings"
	"testing"
)

// TestBackOffice: a Brazilian road carrier's systems behind its node
// (the TMS's name, the MDF-e, the risk manager and toll voucher mocks)
// and the UI's brand line are named in plain text.
func TestBackOffice(t *testing.T) {
	ocean := "  containerPrefix: OCBU\n  vessel: Ocean Blue Aurora\n  voyage: 42W\n  departureHour: 18\n  transit: 134h\n"
	road := `  road: { transit: 30h, cte: { cnpj: "12345678000190", state: 35, mdfe: true }, tms: "TMS (simulated)", risk: { mock: "Risk (simulated)" }, tollVoucher: { mock: "Vouchers (simulated)" } }
`
	in := strings.Replace(strings.Replace(operatorYAML, ocean, road, 1), `listen: ":8443"`, `listen: "127.0.0.1:8443"`, 1) + "web: { auth: demo, brand: \"on a platform\", personas: [{ id: rita, name: Rita, roles: [operator] }] }\n"
	c, err := Parse([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	r := c.Operator.Road
	if !r.CTe.MDFe || r.TMS != "TMS (simulated)" || r.Risk.Mock != "Risk (simulated)" || r.TollVoucher.Mock != "Vouchers (simulated)" || c.Web.Brand != "on a platform" {
		t.Errorf("%+v, brand %q", r, c.Web.Brand)
	}
	for name, edit := range map[string][2]string{
		"an empty risk":          {`risk: { mock: "Risk (simulated)" }`, `risk: { mock: "" }`},
		"a voucher bidi":         {`mock: "Vouchers (simulated)"`, "mock: \"a\u202eb\""},
		"a long tms":             {`tms: "TMS (simulated)"`, `tms: "` + strings.Repeat("x", 65) + `"`},
		"a long brand":           {`brand: "on a platform"`, `brand: "` + strings.Repeat("x", 65) + `"`},
		"systems without a CT-e": {`cte: { cnpj: "12345678000190", state: 35, mdfe: true }, `, ""},
		"an unknown risk key":    {`risk: { mock: "Risk (simulated)" }`, `risk: { url: "https://x" }`},
	} {
		bad := strings.Replace(in, edit[0], edit[1], 1)
		if bad == in {
			t.Fatalf("%s: edit didn't apply", name)
		}
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
