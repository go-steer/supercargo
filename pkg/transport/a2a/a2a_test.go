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
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/go-steer/supercargo/pkg/discovery"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/sig"
	"github.com/go-steer/supercargo/pkg/transport/rest"
)

const env = `{"payload":"e30","protected":"e30","signature":"AA"}`

// recorder is a Processor that records what it got and answers with res.
type recorder struct {
	got []byte
	via bool
	res rest.Result
}

func (r *recorder) Process(ctx context.Context, msg []byte) rest.Result {
	r.got, r.via = msg, Via(ctx)
	return r.res
}

func rpc(params string) string {
	return `{"jsonrpc":"2.0","id":7,"method":"message/send","params":` + params + `}`
}

func sendParams(parts string) string {
	return `{"message":{"kind":"message","messageId":"m1","role":"user","parts":` + parts + `}}`
}

func serve(t *testing.T, h http.Handler, body string) (int, rpcResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/a2a", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var resp rpcResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec.Code, resp
}

func TestHandlerResults(t *testing.T) {
	ok := rpc(sendParams(`[{"kind":"text","text":"Where is it?"},{"kind":"data","data":` + env + `}]`))
	// A signed response comes back in a data part, as it was.
	p := &recorder{res: rest.Result{Response: []byte(`{"payload":"x","protected":"y","signature":"z"}`)}}
	code, resp := serve(t, Handler(p), ok)
	if code != http.StatusOK || resp.Error != nil || string(resp.ID) != "7" {
		t.Fatalf("HTTP %d %+v", code, resp)
	}
	if string(p.got) != env || !p.via {
		t.Errorf("the Processor got %s (via A2A: %v)", p.got, p.via)
	}
	var m Message
	_ = json.Unmarshal(resp.Result, &m)
	if m.Kind != "message" || m.Role != roleAgent || len(m.Parts) != 1 || m.Parts[0].Kind != "data" ||
		string(m.Parts[0].Data) != `{"payload":"x","protected":"y","signature":"z"}` {
		t.Errorf("result %s", resp.Result)
	}
	// Bare refusals: a JSON-RPC error carrying the status, nothing signed.
	for status, wantCode := range map[int]int{
		http.StatusForbidden: errCodeInvalidParams, http.StatusMisdirectedRequest: errCodeInvalidParams,
		http.StatusServiceUnavailable: errCodeUnavailable, http.StatusInternalServerError: errCodeInternal,
	} {
		_, resp := serve(t, Handler(&recorder{res: rest.Result{Status: status}}), ok)
		if resp.Error == nil || resp.Error.Code != wantCode || resp.Result != nil || !strings.Contains(string(resp.Error.Data), `"status":`) {
			t.Errorf("status %d: %+v", status, resp)
		}
	}
	// Accepted with no response: a message with no parts.
	_, resp = serve(t, Handler(&recorder{}), ok)
	if err := json.Unmarshal(resp.Result, &m); err != nil || len(m.Parts) != 0 || resp.Error != nil {
		t.Errorf("no response: %+v", resp)
	}
}

func TestHandlerRejects(t *testing.T) {
	data := `{"kind":"data","data":` + env + `}`
	for name, tc := range map[string]struct {
		body string
		code int
	}{
		"not JSON":           {`{"jsonrpc":`, errCodeParse},
		"duplicate member":   {`{"jsonrpc":"2.0","id":1,"method":"message/send","method":"tasks/get","params":{}}`, errCodeParse},
		"case variant":       {`{"jsonrpc":"2.0","id":1,"Method":"message/send","params":{}}`, errCodeInvalidRequest},
		"not 2.0":            {`{"jsonrpc":"1.0","id":1,"method":"message/send","params":{}}`, errCodeInvalidRequest},
		"no id":              {`{"jsonrpc":"2.0","method":"message/send","params":{}}`, errCodeInvalidRequest},
		"null id":            {`{"jsonrpc":"2.0","id":null,"method":"message/send","params":{}}`, errCodeInvalidRequest},
		"object id":          {`{"jsonrpc":"2.0","id":{},"method":"message/send","params":{}}`, errCodeInvalidRequest},
		"other method":       {`{"jsonrpc":"2.0","id":1,"method":"tasks/get","params":{}}`, errCodeMethodNotFound},
		"no message":         {rpc(`{}`), errCodeInvalidParams},
		"agent message":      {rpc(`{"message":{"kind":"message","messageId":"m","role":"agent","parts":[` + data + `]}}`), errCodeInvalidParams},
		"not a message":      {rpc(`{"message":{"kind":"task","messageId":"m","role":"user","parts":[` + data + `]}}`), errCodeInvalidParams},
		"no data part":       {rpc(sendParams(`[{"kind":"text","text":"hi"}]`)), errCodeInvalidParams},
		"two data parts":     {rpc(sendParams(`[` + data + `,` + data + `]`)), errCodeInvalidParams},
		"data not an object": {rpc(sendParams(`[{"kind":"data","data":"` + "e30" + `"}]`)), errCodeInvalidParams},
		"file part":          {rpc(sendParams(`[` + data + `,{"kind":"file"}]`)), errCodeInvalidParams},
	} {
		p := &recorder{}
		if _, resp := serve(t, Handler(p), tc.body); resp.Error == nil || resp.Error.Code != tc.code {
			t.Errorf("%s: %+v, want JSON-RPC error %d", name, resp, tc.code)
		}
		if p.got != nil {
			t.Errorf("%s: reached the Processor", name)
		}
	}
	// HTTP-level refusals.
	h := Handler(&recorder{})
	for name, req := range map[string]*http.Request{
		"GET":       httptest.NewRequest(http.MethodGet, "/a2a", nil),
		"form post": httptest.NewRequest(http.MethodPost, "/a2a", strings.NewReader("{}")),
		"too large": httptest.NewRequest(http.MethodPost, "/a2a", bytes.NewReader(bytes.Repeat([]byte(" "), maxBody+1))),
	} {
		if name != "form post" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			t.Errorf("%s: HTTP 200", name)
		}
	}
}

func TestCardHandler(t *testing.T) {
	rec := httptest.NewRecorder()
	CardHandler(AgentCard{Name: "Ocean Blue", URL: "https://ocean-blue.example/a2a", Skills: []AgentSkill{{ID: "shipment-inquiry"}}}).
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, WellKnownCardPath, nil))
	var card AgentCard
	if err := json.Unmarshal(rec.Body.Bytes(), &card); err != nil || card.Skills[0].ID != "shipment-inquiry" ||
		rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("card %s, %v", rec.Body.Bytes(), err)
	}
}

type profiles struct {
	doc *discovery.Document
	err error
}

func (p profiles) Profile(context.Context, string) (*discovery.Document, error) { return p.doc, p.err }

func profileFor(t *testing.T, a2aURL string) *discovery.Document {
	t.Helper()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	j, _ := sig.Ed25519JWK(pub, "k1")
	b, _ := json.Marshal(discovery.Profile{
		ProtocolVersion: envelope.Version,
		Party:           discovery.Party{Domain: "ocean-blue.example", Name: "Ocean Blue", Roles: []string{"operator"}},
		Services:        []discovery.Service{{Name: "ulp.freight", Intents: []string{"ulp.tnt.inquiry"}, Extensions: []string{}}},
		Transports:      discovery.Transports{REST: "http://ocean-blue.example/ulp", A2A: a2aURL},
		Keys:            []json.RawMessage{j},
	})
	doc, err := discovery.ParseProfile(b, discovery.ParseOptions{Domain: "ocean-blue.example", AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// clientTo is a Client for ocean-blue.example, whose A2A endpoint is a
// server answering with h.
func clientTo(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := NewClient(profiles{doc: profileFor(t, "http://ocean-blue.example/a2a")},
		rest.ClientOptions{AllowHTTP: true, Peers: map[string]string{"ocean-blue.example": strings.TrimPrefix(srv.URL, "http://")}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// replying answers every request with the JSON-RPC result or error raw.
func replying(t *testing.T, result, rpcErr string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Method != methodMessageSend {
			t.Errorf("request %+v: %v", req, err)
		}
		w.Header().Set("Content-Type", "application/json")
		if rpcErr != "" {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"1","error":` + rpcErr + `}`))
			return
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"1","result":` + result + `}`))
	}
}

func TestClientPost(t *testing.T) {
	// End to end against Handler: the envelope out, the response back.
	p := &recorder{res: rest.Result{Response: []byte(env)}}
	c := clientTo(t, Handler(p).ServeHTTP)
	got, err := c.Post(context.Background(), "ocean-blue.example", []byte(env))
	if err != nil || string(got) != env || string(p.got) != env {
		t.Fatalf("Post = %s, %v (server got %s)", got, err, p.got)
	}
	msg := `{"kind":"message","messageId":"r","role":"agent","parts":[{"kind":"data","data":` + env + `}]}`
	task := `{"kind":"task","id":"t","status":{"state":"completed"},"artifacts":[{"parts":[{"kind":"data","data":` + env + `}]}]}`
	for name, result := range map[string]string{"message": msg, "completed task": task} {
		if got, err := clientTo(t, replying(t, result, "")).Post(context.Background(), "ocean-blue.example", []byte(env)); err != nil || string(got) != env {
			t.Errorf("%s: %s, %v", name, got, err)
		}
	}
	if _, err := clientTo(t, replying(t, `{"kind":"message","messageId":"r","role":"agent","parts":[]}`, "")).
		Post(context.Background(), "ocean-blue.example", []byte(env)); !errors.Is(err, rest.ErrNoResponse) {
		t.Errorf("no parts: %v, want ErrNoResponse", err)
	}
}

func TestClientPostErrors(t *testing.T) {
	post := func(h http.HandlerFunc) error {
		_, err := clientTo(t, h).Post(context.Background(), "ocean-blue.example", []byte(env))
		return err
	}
	var se *rest.StatusError
	if err := post(replying(t, "", `{"code":-32602,"message":"Forbidden","data":{"status":403}}`)); !errors.As(err, &se) || se.Code != 403 {
		t.Errorf("refusal with a status: %v", err)
	}
	if err := post(replying(t, "", `{"code":-32000,"message":"busy"}`)); !errors.As(err, &se) || se.Code != 503 || !se.Retryable() {
		t.Errorf("unavailable: %v", err)
	}
	if err := post(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }); !errors.As(err, &se) || se.Code != 502 {
		t.Errorf("HTTP 502: %v", err)
	}
	for name, h := range map[string]http.HandlerFunc{
		"other JSON-RPC error": replying(t, "", `{"code":-32601,"message":"no"}`),
		"working task":         replying(t, `{"kind":"task","id":"t","status":{"state":"working"}}`, ""),
		"unknown kind":         replying(t, `{"kind":"artifact"}`, ""),
		"result not an object": replying(t, `"done"`, ""),
		"text only":            replying(t, `{"kind":"message","messageId":"r","role":"agent","parts":[{"kind":"text","text":"hi"}]}`, ""),
		"malformed message":    replying(t, `{"kind":"message","parts":"x"}`, ""),
		"wrong id": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"2","result":{}}`))
		},
		"duplicate member": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"1","id":"1","result":{}}`))
		},
		"wrong content type": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) },
		"oversize": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(bytes.Repeat([]byte(" "), maxBody+1))
		},
	} {
		if err := post(h); err == nil || errors.Is(err, rest.ErrNoResponse) {
			t.Errorf("%s: %v, want an error", name, err)
		}
	}
}

type mapDNS map[string][]netip.Addr

func (m mapDNS) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if a, ok := m[host]; ok {
		return a, nil
	}
	return nil, errors.New("no such host")
}

func TestClientEndpoints(t *testing.T) {
	post := func(doc *discovery.Document, perr error, opts rest.ClientOptions) error {
		c, err := NewClient(profiles{doc: doc, err: perr}, opts)
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Post(context.Background(), "ocean-blue.example", []byte(env))
		return err
	}
	if err := post(profileFor(t, ""), nil, rest.ClientOptions{AllowHTTP: true}); !errors.Is(err, ErrNoEndpoint) {
		t.Errorf("no A2A endpoint: %v, want ErrNoEndpoint", err)
	}
	if err := post(profileFor(t, "http://ocean-blue.example/a2a"), nil, rest.ClientOptions{}); err == nil {
		t.Error("an http endpoint without AllowHTTP was used")
	}
	if err := post(nil, errors.New("fetch failed"), rest.ClientOptions{}); err == nil {
		t.Error("a profile error was ignored")
	}
	// The receiver resolving inward: refused by the guard, before any
	// connection.
	inward := rest.ClientOptions{AllowHTTP: true, Resolver: mapDNS{"ocean-blue.example": {netip.MustParseAddr("10.0.0.5")}}}
	if err := post(profileFor(t, "http://ocean-blue.example/a2a"), nil, inward); !errors.Is(err, rest.ErrRefused) {
		t.Errorf("an inward-resolving endpoint: %v, want ErrRefused", err)
	}
	// Nothing listening: a transport failure, worth retrying.
	dead := rest.ClientOptions{AllowHTTP: true, Peers: map[string]string{"ocean-blue.example": "127.0.0.1:1"}}
	var te *rest.TransportError
	if err := post(profileFor(t, "http://ocean-blue.example/a2a"), nil, dead); !errors.As(err, &te) {
		t.Errorf("nothing listening: %v, want a TransportError", err)
	}
	if _, err := NewClient(profiles{}, rest.ClientOptions{Peers: map[string]string{"x.example": "no port"}}); err == nil {
		t.Error("a bad peer map was accepted")
	}
}
