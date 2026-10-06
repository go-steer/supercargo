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

package a2a

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/go-steer/supercargo/pkg/discovery"
	"github.com/go-steer/supercargo/pkg/transport/rest"
)

// TestEnvelopesGoVerbatim: the envelope reaches the Processor, and the
// response reaches the caller, byte for byte, whitespace and characters
// encoding/json would escape included.
func TestEnvelopesGoVerbatim(t *testing.T) {
	odd := `{ "payload" : "e30",  "protected":"<&>", "signature":"AA" }`
	reply := `{"payload":"x" ,"protected":"<y>","signature":"z&"}`
	p := &recorder{res: rest.Result{Response: []byte(reply)}}
	got, err := clientTo(t, Handler(p).ServeHTTP).Post(context.Background(), "ocean-blue.example", []byte(odd))
	if err != nil {
		t.Fatal(err)
	}
	if string(p.got) != odd {
		t.Errorf("the Processor got %s, want %s", p.got, odd)
	}
	if string(got) != reply {
		t.Errorf("the caller got %s, want %s", got, reply)
	}
	if _, err := clientTo(t, Handler(p).ServeHTTP).Post(context.Background(), "ocean-blue.example", []byte(`"not an object"`)); err == nil {
		t.Error("a non-object envelope was sent")
	}
}

func TestHandlerMixedPartsAndStatuses(t *testing.T) {
	for name, parts := range map[string]string{
		"text part with data": `[{"kind":"text","text":"hi","data":` + env + `},{"kind":"data","data":` + env + `}]`,
		"data part with text": `[{"kind":"data","text":"hi","data":` + env + `}]`,
	} {
		p := &recorder{}
		if _, resp := serve(t, Handler(p), rpc(sendParams(parts))); resp.Error == nil || resp.Error.Code != errCodeInvalidParams || p.got != nil {
			t.Errorf("%s: %+v", name, resp)
		}
	}
	// A status outside 4xx/5xx is reported as 500, as the REST binding does.
	_, resp := serve(t, Handler(&recorder{res: rest.Result{Status: http.StatusFound}}), rpc(sendParams(`[{"kind":"data","data":`+env+`}]`)))
	var d struct{ Status int }
	_ = json.Unmarshal(resp.Error.Data, &d)
	if resp.Error.Code != errCodeInternal || d.Status != http.StatusInternalServerError {
		t.Errorf("302 from the Processor: %+v", resp.Error)
	}
}

// TestClientEndpointChecks: the client's own checks on a profile's A2A
// endpoint, behind the profile parser's (a Document built directly skips
// those).
func TestClientEndpointChecks(t *testing.T) {
	for _, endpoint := range []string{
		"https://mallory.example/a2a",
		"https://ocean-blue.example:8443/a2a",
		"https://user@ocean-blue.example/a2a",
		"ftp://ocean-blue.example/a2a",
		"%zz",
	} {
		doc := &discovery.Document{Profile: discovery.Profile{Transports: discovery.Transports{A2A: endpoint}}}
		c, err := NewClient(profiles{doc: doc}, rest.ClientOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Post(context.Background(), "ocean-blue.example", []byte(env)); err == nil || !strings.Contains(err.Error(), "not usable") {
			t.Errorf("endpoint %s: %v, want refused", endpoint, err)
		}
	}
}
