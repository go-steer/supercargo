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

package rest

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
	"sync/atomic"
	"testing"

	"github.com/go-steer/supercargo/pkg/discovery"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/sig"
)

type processorFunc func(context.Context, []byte) Result

func (f processorFunc) Process(ctx context.Context, msg []byte) Result { return f(ctx, msg) }

func TestHandler(t *testing.T) {
	var got []byte
	h := Handler(processorFunc(func(_ context.Context, msg []byte) Result {
		got = msg
		switch string(msg) {
		case "malformed":
			return Result{Status: http.StatusBadRequest}
		case "failed":
			return Result{Status: http.StatusInternalServerError}
		case "misdirected":
			return Result{Status: http.StatusMisdirectedRequest}
		case "forbidden":
			return Result{Status: http.StatusForbidden}
		case "unavailable":
			return Result{Status: http.StatusServiceUnavailable}
		case "bogus status":
			return Result{Status: http.StatusOK}
		case "push":
			return Result{}
		}
		return Result{Response: []byte(`{"reply":true}`)}
	}))
	do := func(method, ct, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/ulp", strings.NewReader(body))
		if ct != "" {
			r.Header.Set("Content-Type", ct)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := do(http.MethodPost, "application/json", "request"); w.Code != 200 || w.Body.String() != `{"reply":true}` ||
		w.Header().Get("Content-Type") != ContentType || string(got) != "request" {
		t.Errorf("request: %d %q", w.Code, w.Body)
	}
	if w := do(http.MethodPost, "application/json; charset=utf-8", "request"); w.Code != 200 {
		t.Errorf("content type with parameters: %d", w.Code)
	}
	for _, tc := range []struct {
		method, ct, body string
		status           int
	}{
		{http.MethodPost, "application/json", "malformed", 400},
		{http.MethodPost, "application/json", "failed", 500},
		{http.MethodPost, "application/json", "misdirected", 421},
		{http.MethodPost, "application/json", "forbidden", 403},
		{http.MethodPost, "application/json", "unavailable", 503},
		{http.MethodPost, "application/json", "bogus status", 500},
		{http.MethodPost, "application/json", "push", 204},
		{http.MethodGet, "", "", 405},
		{http.MethodPut, "application/json", "x", 405},
		{http.MethodPost, "text/plain", "x", 415},
		{http.MethodPost, "", "x", 415},
		{http.MethodPost, "application/json", strings.Repeat("x", envelope.MaxSize+1), 413},
	} {
		w := do(tc.method, tc.ct, tc.body)
		if w.Code != tc.status {
			t.Errorf("%s %s (%d bytes): %d, want %d", tc.method, tc.ct, len(tc.body), w.Code, tc.status)
		}
		if tc.status != 200 && w.Code >= 400 && strings.Contains(w.Body.String(), "reply") {
			t.Errorf("%d carried a response body", w.Code)
		}
	}
}

// profiles serves one fixed profile for every domain.
type profiles struct {
	doc *discovery.Document
	err error
}

func (p profiles) Profile(context.Context, string) (*discovery.Document, error) { return p.doc, p.err }

func profileFor(t *testing.T, domain, rest string, allowHTTP bool) *discovery.Document {
	t.Helper()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	j, _ := sig.Ed25519JWK(pub, "k1")
	b, _ := json.Marshal(discovery.Profile{
		ProtocolVersion: envelope.Version,
		Party:           discovery.Party{Domain: domain, Name: "Ocean Blue", Roles: []string{"operator"}},
		Services:        []discovery.Service{{Name: "ulp.freight", Intents: []string{"ulp.booking.request"}, Extensions: []string{}}},
		Transports:      discovery.Transports{REST: rest},
		Keys:            []json.RawMessage{j},
	})
	doc, err := discovery.ParseProfile(b, discovery.ParseOptions{Domain: domain, AllowHTTP: allowHTTP})
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func client(t *testing.T, h http.HandlerFunc, allowHTTP bool) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := NewClient(profiles{doc: profileFor(t, "ocean-blue.example", "http://ocean-blue.example/ulp", true)},
		ClientOptions{AllowHTTP: allowHTTP, Peers: map[string]string{"ocean-blue.example": strings.TrimPrefix(srv.URL, "http://")}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestClientPost(t *testing.T) {
	var sent []byte
	c := client(t, func(w http.ResponseWriter, r *http.Request) {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(r.Body)
		sent = buf.Bytes()
		if r.Header.Get("Content-Type") != ContentType || r.Method != http.MethodPost || r.URL.Path != "/ulp" {
			t.Errorf("request: %s %s %s", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
		}
		w.Header().Set("Content-Type", ContentType)
		_, _ = w.Write([]byte(`{"ok":1}`))
	}, true)
	resp, err := c.Post(context.Background(), "ocean-blue.example", []byte(`{"m":1}`))
	if err != nil || string(resp) != `{"ok":1}` || string(sent) != `{"m":1}` {
		t.Errorf("Post = %q, %v (sent %q)", resp, err, sent)
	}
}

type mapDNS map[string][]netip.Addr

func (m mapDNS) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if a, ok := m[host]; ok {
		return a, nil
	}
	return nil, errors.New("no such host")
}

// TestPostTo: a webhook goes exactly to the registered endpoint, which must
// be on the receiver's own domain and reachable through the guard.
func TestPostTo(t *testing.T) {
	var path string
	var hits atomic.Int32
	c := client(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}, true)
	if _, err := c.PostTo(context.Background(), "ocean-blue.example", "http://ocean-blue.example/hooks/ulp", nil); !errors.Is(err, ErrNoResponse) {
		t.Fatalf("PostTo = %v, want ErrNoResponse (204)", err)
	}
	if path != "/hooks/ulp" {
		t.Errorf("posted to %s", path)
	}
	for endpoint, want := range map[string]string{
		"http://mallory.example/ulp":         "is not on",
		"http://ocean-blue.example:8080/ulp": "is not on",
		"http://user@ocean-blue.example/ulp": "is not on",
		"http://OCEAN-BLUE.example/ulp":      "is not on",
		"ftp://ocean-blue.example/ulp":       "is not https",
		"%zz":                                "invalid URL escape",
	} {
		if _, err := c.PostTo(context.Background(), "ocean-blue.example", endpoint, nil); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("PostTo(%s) = %v, want %q", endpoint, err, want)
		}
	}
	if hits.Load() != 1 {
		t.Errorf("the server was hit %d times, want 1", hits.Load())
	}
	// The receiver's own domain, resolving inward: refused by the guard,
	// before any connection.
	guarded, _ := NewClient(profiles{}, ClientOptions{AllowHTTP: true, Resolver: mapDNS{"cymbal-shoes.example": {netip.MustParseAddr("10.0.0.5")}}})
	if _, err := guarded.PostTo(context.Background(), "cymbal-shoes.example", "http://cymbal-shoes.example/ulp", nil); !errors.Is(err, ErrRefused) {
		t.Errorf("PostTo an inward-resolving host = %v, want ErrRefused", err)
	}
}

func TestClientPostRejects(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"204": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) },
		"400": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadRequest) },
		"redirect": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://169.254.169.254/", http.StatusTemporaryRedirect)
		},
		"wrong content type": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) },
		"oversize": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", ContentType)
			_, _ = w.Write(bytes.Repeat([]byte("x"), envelope.MaxSize+1))
		},
	} {
		t.Run(name, func(t *testing.T) {
			resp, err := client(t, h, true).Post(context.Background(), "ocean-blue.example", []byte("{}"))
			if err == nil {
				t.Errorf("Post = %d bytes, want an error", len(resp))
			}
			if name == "204" && !errors.Is(err, ErrNoResponse) {
				t.Errorf("204: %v, want ErrNoResponse", err)
			}
		})
	}
	// An http endpoint is refused unless the client is in insecure mode,
	// even though the server would answer perfectly well, and whatever the
	// scheme's case.
	called := false
	good := func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.Header().Set("Content-Type", ContentType)
		_, _ = w.Write([]byte("{}"))
	}
	for _, endpoint := range []string{"http://ocean-blue.example/ulp", "HTTP://ocean-blue.example/ulp"} {
		srv := httptest.NewServer(http.HandlerFunc(good))
		c, _ := NewClient(profiles{doc: profileFor(t, "ocean-blue.example", endpoint, true)},
			ClientOptions{Peers: map[string]string{"ocean-blue.example": strings.TrimPrefix(srv.URL, "http://")}})
		if _, err := c.Post(context.Background(), "ocean-blue.example", nil); err == nil || called {
			t.Errorf("posted to %s without AllowHTTP (err %v)", endpoint, err)
		}
		srv.Close()
	}
	// A refusal with a proper JSON body is still a refusal.
	_, err := client(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", ContentType)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"protected":"e30","payload":"e30","signature":"AA"}`))
	}, true).Post(context.Background(), "ocean-blue.example", nil)
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 503 || !se.Retryable() || !strings.Contains(se.Error(), "503") {
		t.Errorf("503 with a body = %v, want a retryable StatusError", err)
	}
	if (&StatusError{Code: 403}).Retryable() {
		t.Error("403 reported retryable")
	}
	// No profile, no post.
	c, _ := NewClient(profiles{err: errors.New("unavailable")}, ClientOptions{})
	if _, err := c.Post(context.Background(), "ocean-blue.example", nil); err == nil {
		t.Error("posted without a profile")
	}
	if _, err := NewClient(profiles{}, ClientOptions{Peers: map[string]string{"Bad Key": "x:1"}}); err == nil {
		t.Error("NewClient accepted an invalid peer map")
	}
	// Without peers, the endpoint's host must be public: here it doesn't
	// resolve at all, offline.
	c, _ = NewClient(profiles{doc: profileFor(t, "ocean-blue.example", "https://ocean-blue.example/ulp", false)},
		ClientOptions{Resolver: noDNS{}})
	if _, err := c.Post(context.Background(), "ocean-blue.example", nil); err == nil {
		t.Error("posted to an unresolvable host")
	}
}

// TestClientHeader: the Header hook adds headers from the request's
// context, and can't change the content type.
func TestClientHeader(t *testing.T) {
	type key struct{}
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", ContentType)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	c, err := NewClient(profiles{doc: profileFor(t, "ocean-blue.example", "http://ocean-blue.example/ulp", true)},
		ClientOptions{AllowHTTP: true, Peers: map[string]string{"ocean-blue.example": strings.TrimPrefix(srv.URL, "http://")},
			Header: func(ctx context.Context, h http.Header) {
				h.Set("Traceparent", ctx.Value(key{}).(string))
				h.Set("Content-Type", "text/evil")
			}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Post(context.WithValue(context.Background(), key{}, "00-abc-def-01"), "ocean-blue.example", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if got.Get("Traceparent") != "00-abc-def-01" || got.Get("Content-Type") != ContentType {
		t.Errorf("headers %v", got)
	}
}
