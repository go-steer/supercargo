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

// Package rest is ULP's REST binding (docs/DESIGN.md, section A.7): a
// serialized envelope POSTed as the request body to a party's ULP endpoint,
// and its signed response, if any, as the response body.
//
// The binding moves bytes and nothing else. Every trust decision is made
// on the envelope by whatever Processor the server hands it to, and by the
// sender on the response, so nothing here depends on the hop.
//
// HTTP status codes report transport outcomes only. Every refusal decided
// before the sender is verified is a bare status with no body, because
// signing an answer to an unverified message would let anyone obtain signed
// statements of their choosing.
//   - 200: the body is a signed response envelope (which may be a ulp.error)
//   - 204: accepted, no response (pushed events and alerts)
//   - 400: not an envelope, or an intent the receiver doesn't serve
//   - 403: the sender's key is unknown, or the signature doesn't verify
//   - 421: the envelope is addressed to another party
//   - 503: the sender's profile can't be fetched right now (retry later)
//   - 405, 413, 415: wrong method, too large, wrong content type
//   - 500: the receiver couldn't produce a response at all
package rest

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"time"

	"github.com/go-steer/supercargo/pkg/discovery"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/internal/logsafe"
	"github.com/go-steer/supercargo/pkg/internal/netguard"
)

// ContentType is the media type of envelopes in both directions.
const ContentType = "application/json"

// Result is what a Processor decided about one inbound message: either a
// signed Response (200), or a bare Status with no body. A zero Result means
// accepted with no response (204).
type Result struct {
	Response []byte
	Status   int
}

// Processor handles one inbound serialized envelope.
type Processor interface {
	Process(ctx context.Context, msg []byte) Result
}

// Handler serves POST requests carrying envelopes. Mount it at the path the
// party's profile publishes as transports.rest (conventionally /ulp).
func Handler(p Processor) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != ContentType {
			http.Error(w, "content type must be "+ContentType, http.StatusUnsupportedMediaType)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, envelope.MaxSize))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(w, "envelope too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "reading the request failed", http.StatusBadRequest)
			return
		}
		res := p.Process(r.Context(), body)
		switch {
		case res.Response != nil:
			w.Header().Set("Content-Type", ContentType)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(res.Response)
		case res.Status >= 400 && res.Status <= 599:
			w.WriteHeader(res.Status)
		case res.Status != 0:
			// Anything else would claim an outcome the binding doesn't define.
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
}

// Profiles finds a party's profile. *discovery.Cache implements it.
type Profiles interface {
	Profile(ctx context.Context, domain string) (*discovery.Document, error)
}

// ClientOptions configures a Client. They mean what they mean for
// discovery.FetcherOptions, and a node passes both the same values.
type ClientOptions struct {
	// Peers maps a domain to the host:port to connect to for it (local
	// deployments only); every other host must resolve to public addresses.
	Peers map[string]string
	// AllowHTTP permits http endpoints (insecure development mode only).
	AllowHTTP bool
	// TLSConfig adds trusted roots (a local CA).
	TLSConfig *tls.Config
	// Timeout bounds one exchange. Zero means 10s.
	Timeout time.Duration
	// Resolver replaces net.DefaultResolver (tests).
	Resolver netguard.Resolver
}

// Client posts envelopes to other parties' REST endpoints. Its HTTP client
// is always the guarded one: endpoints come from partners' profiles, so
// they are hosts named by someone else.
type Client struct {
	http      *http.Client
	profiles  Profiles
	allowHTTP bool
}

// NewClient returns a Client that finds endpoints in profiles.
func NewClient(profiles Profiles, opts ClientOptions) (*Client, error) {
	hc, err := netguard.NewClient(netguard.Options{
		Peers:     opts.Peers,
		TLSConfig: opts.TLSConfig,
		Timeout:   opts.Timeout,
		Resolver:  opts.Resolver,
	})
	if err != nil {
		return nil, fmt.Errorf("rest: %w", err)
	}
	return &Client{http: hc, profiles: profiles, allowHTTP: opts.AllowHTTP}, nil
}

// ErrNoResponse reports a 204: the receiver accepted the message and sent
// nothing back.
var ErrNoResponse = errors.New("rest: accepted with no response")

// ErrRefused reports an endpoint the guarded client won't connect to: its
// host resolves to a non-public address. Retrying won't help.
var ErrRefused = netguard.ErrRefused

// TransportError is a failure to exchange HTTP at all (connection refused,
// reset, timeout, TLS failure): the receiver may be fine a moment later.
type TransportError struct {
	Receiver string
	Err      error
}

func (e *TransportError) Error() string {
	return fmt.Sprintf("rest: posting to %s: %v", e.Receiver, e.Err)
}

func (e *TransportError) Unwrap() error { return e.Err }

// StatusError is a bare HTTP refusal: the receiver decided before (or
// without) verifying the message, or couldn't answer at all.
type StatusError struct {
	Receiver string
	Code     int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("rest: %s answered HTTP %d (%s)", e.Receiver, e.Code, http.StatusText(e.Code))
}

// Retryable reports a refusal worth retrying later: 503 (the receiver
// couldn't fetch the sender's profile, or is overloaded), any other 5xx,
// 408 and 429.
func (e *StatusError) Retryable() bool {
	return e.Code >= 500 || e.Code == http.StatusRequestTimeout || e.Code == http.StatusTooManyRequests
}

// Post sends a serialized envelope to receiver's REST endpoint and returns
// the response envelope, unverified: checking it is the caller's job. A 204
// returns ErrNoResponse.
func (c *Client) Post(ctx context.Context, receiver string, msg []byte) ([]byte, error) {
	doc, err := c.profiles.Profile(ctx, receiver)
	if err != nil {
		return nil, fmt.Errorf("rest: %w", err)
	}
	return c.PostTo(ctx, receiver, doc.Profile.Transports.REST, msg)
}

// PostTo sends a serialized envelope to endpoint, which must be on
// receiver's own domain, and returns the response like Post. It is for
// webhooks: a subscriber registers the endpoint pushes go to. The request
// still goes through the guarded client, so an endpoint whose host
// resolves to a non-public address is refused.
func (c *Client) PostTo(ctx context.Context, receiver, endpoint string, msg []byte) ([]byte, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("rest: %s's endpoint %s: %w", receiver, logsafe.Quote(endpoint), err)
	}
	// url.Parse lowercases the scheme, so "HTTP://" is caught too. Profiles
	// parsed in insecure mode may name http endpoints; a client not in that
	// mode must not follow them.
	if u.Scheme != "https" && (!c.allowHTTP || u.Scheme != "http") {
		return nil, fmt.Errorf("rest: %s's endpoint %s is not https", receiver, logsafe.Quote(endpoint))
	}
	if u.Hostname() != receiver || u.Host != u.Hostname() || u.User != nil {
		// Profiles and subscriptions are already validated this way; a
		// post never goes anywhere but the receiver's own domain.
		return nil, fmt.Errorf("rest: endpoint %s is not on %s", logsafe.Quote(endpoint), receiver)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(msg))
	if err != nil {
		return nil, fmt.Errorf("rest: %w", err)
	}
	req.Header.Set("Content-Type", ContentType)
	req.Header.Set("Accept", ContentType)
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, ErrRefused) {
			return nil, fmt.Errorf("rest: posting to %s: %w", receiver, err)
		}
		return nil, &TransportError{Receiver: receiver, Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNoContent:
		return nil, ErrNoResponse
	default:
		// Drain a little, so the connection can be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, &StatusError{Receiver: receiver, Code: resp.StatusCode}
	}
	if mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err != nil || mt != ContentType {
		return nil, fmt.Errorf("rest: %s answered with content type %s", receiver, logsafe.Quote(resp.Header.Get("Content-Type")))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, envelope.MaxSize+1))
	if err != nil {
		return nil, fmt.Errorf("rest: reading the response from %s: %w", receiver, err)
	}
	if len(body) > envelope.MaxSize {
		return nil, fmt.Errorf("rest: the response from %s is over %d bytes", receiver, envelope.MaxSize)
	}
	return body, nil
}
