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

package store

import (
	"encoding/json"
	"testing"

	"github.com/go-steer/supercargo/pkg/intents"
)

// TestViewRecordsSource: a record view says whether a re-issued record's
// sources were all signed, from its body.
func TestViewRecordsSource(t *testing.T) {
	body := func(src ...intents.Provenance) json.RawMessage {
		b, _ := json.Marshal(intents.TnTEventBody{EventID: "EV-1", DerivedFrom: src})
		return b
	}
	views := ViewRecords([]Record{
		{ID: "a", Intent: intents.TnTEvent, Body: body(intents.Provenance{ID: "EV-9", Signed: true})},
		{ID: "b", Intent: intents.TnTEvent, Body: body(intents.Provenance{ID: "EV-9", Signed: true}, intents.Provenance{Source: "EDIFACT", Signed: false})},
		{ID: "c", Intent: intents.TnTEvent, Body: body()},
		{ID: "d", Intent: intents.TnTEvent, Body: json.RawMessage(`not json`)},
	})
	for i, want := range []string{SourceSigned, SourceUnsigned, "", ""} {
		if views[i].Source != want {
			t.Errorf("record %d: %q, want %q", i, views[i].Source, want)
		}
	}
}
