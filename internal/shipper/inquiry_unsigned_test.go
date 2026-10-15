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
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/intents"
)

// TestAskUnsigned: of the cited records this shipper holds, the ones its
// own copies say came from an unsigned source are listed as such; what it
// doesn't hold, or holds as signed, isn't, whatever the answer says.
func TestAskUnsigned(t *testing.T) {
	s := answering(t, "Picked up, then shipped.", []string{"EV-1", "EV-2", "EV-3", "EV-9"}, "")
	add := func(id string, src ...intents.Provenance) {
		raw, _ := json.Marshal(intents.TnTEventBody{EventID: id, DerivedFrom: src})
		if _, err := s.Events.Add("ocean-blue.example", "OB-BKG-1", store.Record{ID: id, Intent: intents.TnTEvent, Body: raw}); err != nil {
			t.Fatal(err)
		}
	}
	add("EV-2", intents.Provenance{Source: "EDIFACT IFTSTA from polder-haulage.example", Signed: false})
	add("EV-3", intents.Provenance{ID: "EV-000003", Signed: true})
	a, err := s.Ask(context.Background(), "ocean-blue.example", "OB-BKG-1", "Where is it?")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(a.Held, ",") != "EV-1,EV-2,EV-3" || strings.Join(a.Unsigned, ",") != "EV-2" {
		t.Errorf("held %v, unsigned %v", a.Held, a.Unsigned)
	}
	// An unsigned alert counts; an unsigned record of another booking, under
	// the cited ID, doesn't.
	alert, _ := json.Marshal(intents.TnTAlertBody{AlertID: "AL-4", DerivedFrom: []intents.Provenance{{Source: "EDIFACT IFTSTA", Signed: false}}})
	if _, err := s.Events.Add("ocean-blue.example", "OB-BKG-1", store.Record{ID: "AL-4", Intent: intents.TnTAlert, Body: alert}); err != nil {
		t.Fatal(err)
	}
	other, _ := json.Marshal(intents.TnTEventBody{EventID: "EV-9", DerivedFrom: []intents.Provenance{{Source: "EDIFACT IFTSTA", Signed: false}}})
	if _, err := s.Events.Add("ocean-blue.example", "OB-BKG-2", store.Record{ID: "EV-9", Intent: intents.TnTEvent, Body: other}); err != nil {
		t.Fatal(err)
	}
	s2 := answering(t, "Late.", []string{"EV-2", "AL-4", "EV-9"}, "")
	s2.Events = s.Events
	a, err = s2.Ask(context.Background(), "ocean-blue.example", "OB-BKG-1", "Why?")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(a.Unsigned, ",") != "EV-2,AL-4" {
		t.Errorf("unsigned %v", a.Unsigned)
	}
}
