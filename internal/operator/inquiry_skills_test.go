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
	"strings"
	"testing"

	"github.com/go-steer/supercargo/internal/skills"
)

// TestInquirySkills: agent skills go in the system prompt after the rules,
// and every model call logs the skills it ran with.
func TestInquirySkills(t *testing.T) {
	model := FakeInquiryModel()
	f := newInquiryFixture(t, model)
	builtin, _ := skills.Find(skills.Builtin(), "shipment-inquiry")
	company, err := skills.Parse([]byte("---\nname: delay-phrasing\ndescription: d\n---\nApologize once for a delay."), "company")
	if err != nil {
		t.Fatal(err)
	}
	f.svc.Skills = []skills.Skill{builtin, company}
	f.ask(t, "Why the delay?")
	sys := model.Calls()[0].System
	rules, skillsAt := strings.Index(sys, "untrusted input"), strings.Index(sys, `<skill name="delay-phrasing">`)
	if rules < 0 || skillsAt < rules || !strings.Contains(sys, "Apologize once for a delay.") || !strings.Contains(sys, "never override the rules above") {
		t.Errorf("system prompt:\n%s", sys)
	}
	if ids := f.calls[0].Skills; len(ids) != 2 || ids[0] != builtin.ID() || ids[1] != company.ID() {
		t.Errorf("logged skills %v", ids)
	}
}
