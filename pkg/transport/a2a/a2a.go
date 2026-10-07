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

// Package a2a is ULP's A2A binding (docs/DESIGN.md, section A.7), for the
// agentic skills: one company's agent delegating an open-ended task, such
// as an inquiry, to another's.
//
// A request is a JSON-RPC message/send whose message carries one data part:
// the full signed envelope. The reply carries the signed response envelope
// in a data part. Like the REST binding it moves bytes and nothing else:
// every trust decision is made on the envelope, by the same Processor, so
// a message is verified, authorized and answered the same way whichever
// binding carried it. A refusal made before the sender is verified is a
// JSON-RPC error carrying the bare HTTP status the REST binding would have
// answered with, and nothing signed.
//
// The official A2A Go SDK was evaluated (as go-steer/mast did before it):
// the binding needs one method and one part kind, so a small stdlib
// implementation keeps the SDK's dependency tree out of pkg/.
package a2a

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/internal/logsafe"
	"github.com/go-steer/supercargo/pkg/internal/netguard"
	"github.com/go-steer/supercargo/pkg/internal/strictjson"
	"github.com/go-steer/supercargo/pkg/transport/rest"
)

// maxBody bounds a JSON-RPC request or response: an envelope plus the
// JSON-RPC and A2A wrapping around it.
const maxBody = envelope.MaxSize + 16<<10

type viaKey struct{}

// Via reports whether ctx belongs to a message that arrived over A2A, so a
// Processor can log the binding.
func Via(ctx context.Context) bool {
	v, _ := ctx.Value(viaKey{}).(bool)
	return v
}

// Handler serves message/send at the party's A2A endpoint (conventionally
// /a2a), handing the envelope in the request's data part to p.
func Handler(p rest.Processor) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
			http.Error(w, "content type must be application/json", http.StatusUnsupportedMediaType)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "reading the request failed", http.StatusBadRequest)
			return
		}
		id, msg, rpcErr := parseRequest(body)
		if rpcErr != nil {
			answer(w, id, nil, rpcErr)
			return
		}
		res := p.Process(context.WithValue(r.Context(), viaKey{}, true), msg)
		switch {
		case res.Response != nil:
			// Written out by hand: encoding/json would re-encode the signed
			// envelope (compacting it, escaping <, > and &), and it goes on
			// as it was produced.
			var out bytes.Buffer
			out.WriteString(`{"jsonrpc":"2.0","id":`)
			out.Write(id)
			out.WriteString(`,"result":`)
			out.Write(dataMessage(roleAgent, res.Response))
			out.WriteString("}\n")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(out.Bytes())
		case res.Status != 0:
			status := res.Status
			if status < 400 || status > 599 {
				// Anything else would claim an outcome the binding doesn't
				// define (as in the REST binding).
				status = http.StatusInternalServerError
			}
			code := errCodeInvalidParams
			switch {
			case status == http.StatusServiceUnavailable:
				code = errCodeUnavailable
			case status >= 500:
				code = errCodeInternal
			}
			res.Status = status
			answer(w, id, nil, &RPCError{Code: code, Message: http.StatusText(res.Status),
				Data: json.RawMessage(`{"status":` + strconv.Itoa(res.Status) + `}`)})
		default:
			// Accepted with no response: a message with no parts.
			answer(w, id, &Message{Kind: "message", MessageID: newID(), Role: roleAgent, Parts: []Part{}}, nil)
		}
	})
}

// dataMessage is an A2A message from role with one data part carrying
// data verbatim.
func dataMessage(role string, data []byte) []byte {
	var b bytes.Buffer
	b.WriteString(`{"kind":"message","messageId":"` + newID() + `","role":"` + role + `","parts":[{"kind":"data","data":`)
	b.Write(data)
	b.WriteString(`}]}`)
	return b.Bytes()
}

// parseRequest checks a JSON-RPC message/send request and returns its id
// and the envelope in its one data part. A request without an id (a
// JSON-RPC notification) is refused with an error rather than ignored: an
// envelope sent without waiting for its answer has no place here. The request is unauthenticated, so
// it is parsed strictly: duplicate or case-variant members could make two
// readers see two different envelopes.
func parseRequest(body []byte) (json.RawMessage, []byte, *RPCError) {
	nullID := json.RawMessage("null")
	if err := strictjson.Valid(body); err != nil {
		return nullID, nil, &RPCError{Code: errCodeParse, Message: "not valid JSON"}
	}
	var req rpcRequest
	if err := strictjson.UnmarshalKnown(body, &req); err != nil {
		return nullID, nil, &RPCError{Code: errCodeInvalidRequest, Message: "not a JSON-RPC request"}
	}
	if req.JSONRPC != "2.0" || !validID(req.ID) {
		return nullID, nil, &RPCError{Code: errCodeInvalidRequest, Message: "not a JSON-RPC 2.0 request with an id"}
	}
	if req.Method != methodMessageSend {
		return req.ID, nil, &RPCError{Code: errCodeMethodNotFound, Message: "only message/send is served"}
	}
	var params messageSendParams
	if err := strictjson.UnmarshalKnown(req.Params, &params); err != nil || params.Message == nil {
		return req.ID, nil, &RPCError{Code: errCodeInvalidParams, Message: "params.message is required"}
	}
	m := params.Message
	if m.Kind != "message" || m.Role != roleUser {
		return req.ID, nil, &RPCError{Code: errCodeInvalidParams, Message: "the message must be a user message"}
	}
	data, err := onlyData(m.Parts)
	if err != nil {
		return req.ID, nil, &RPCError{Code: errCodeInvalidParams, Message: err.Error()}
	}
	return req.ID, data, nil
}

// onlyData returns the one data part's data, which must be a JSON object
// (an envelope). Text parts, a rendering for people, are ignored.
func onlyData(parts []Part) ([]byte, error) {
	var data []byte
	for _, p := range parts {
		switch p.Kind {
		case "data":
			if data != nil {
				return nil, errors.New("more than one data part")
			}
			if p.Text != "" {
				return nil, errors.New("a data part with text")
			}
			if len(p.Data) == 0 || p.Data[0] != '{' {
				return nil, errors.New("the data part must be a signed envelope")
			}
			data = p.Data
		case "text":
			// A text part is a rendering for people; one that also carries
			// data could show an observer another envelope.
			if len(p.Data) > 0 {
				return nil, errors.New("a text part with data")
			}
		default:
			return nil, fmt.Errorf("unsupported part kind %s", logsafe.Quote(p.Kind))
		}
	}
	if data == nil {
		return nil, errors.New("no data part with an envelope")
	}
	return data, nil
}

// validID accepts a JSON-RPC id that is a string or a number.
func validID(id json.RawMessage) bool {
	if len(id) == 0 {
		return false
	}
	c := id[0]
	return c == '"' || c == '-' || (c >= '0' && c <= '9')
}

func answer(w http.ResponseWriter, id json.RawMessage, result *Message, rpcErr *RPCError) {
	resp := rpcResponse{JSONRPC: "2.0", ID: id, Error: rpcErr}
	if result != nil {
		resp.Result, _ = json.Marshal(result)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// CardHandler serves card at WellKnownCardPath.
func CardHandler(card AgentCard) http.Handler {
	b, err := json.Marshal(card)
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if err != nil {
			http.Error(w, "card unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	})
}

// ErrNoEndpoint reports a receiver whose profile publishes no A2A
// endpoint: the message can go over REST instead.
var ErrNoEndpoint = errors.New("a2a: the receiver publishes no A2A endpoint")

// Client sends envelopes to other parties' A2A endpoints, found in their
// profiles, always through the guarded outbound client.
type Client struct {
	http      *http.Client
	profiles  rest.Profiles
	allowHTTP bool
	timeout   time.Duration
	header    func(context.Context, http.Header)
}

// NewClient returns a Client. The options mean what they mean for the REST
// client, and a node passes both the same values.
func NewClient(profiles rest.Profiles, opts rest.ClientOptions) (*Client, error) {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	hc, err := netguard.NewClient(netguard.Options{
		Peers: opts.Peers, TLSConfig: opts.TLSConfig, Timeout: max(opts.MaxTimeout, timeout), Resolver: opts.Resolver,
	})
	if err != nil {
		return nil, fmt.Errorf("a2a: %w", err)
	}
	return &Client{http: hc, profiles: profiles, allowHTTP: opts.AllowHTTP, timeout: timeout, header: opts.Header}, nil
}

// Post sends a serialized envelope to receiver's A2A endpoint and returns
// the response envelope, unverified: checking it is the caller's job. Its
// errors are the REST client's (rest.StatusError, rest.TransportError,
// rest.ErrNoResponse, rest.ErrRefused), so callers treat both alike.
func (c *Client) Post(ctx context.Context, receiver string, msg []byte) ([]byte, error) {
	doc, err := c.profiles.Profile(ctx, receiver)
	if err != nil {
		return nil, fmt.Errorf("a2a: %w", err)
	}
	endpoint := doc.Profile.Transports.A2A
	if endpoint == "" {
		return nil, ErrNoEndpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "https" && (!c.allowHTTP || u.Scheme != "http")) ||
		u.Hostname() != receiver || u.Host != u.Hostname() || u.User != nil {
		return nil, fmt.Errorf("a2a: %s's endpoint %s is not usable", receiver, logsafe.Quote(endpoint))
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	if !json.Valid(msg) || len(msg) == 0 || msg[0] != '{' {
		return nil, errors.New("a2a: the envelope is not a JSON object")
	}
	// Written out by hand, so the envelope goes as it was sealed.
	var b bytes.Buffer
	b.WriteString(`{"jsonrpc":"2.0","id":"1","method":"` + methodMessageSend + `","params":{"message":`)
	b.Write(dataMessage(roleUser, msg))
	b.WriteString(`}}`)
	body := b.Bytes()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("a2a: %w", err)
	}
	if c.header != nil {
		c.header(ctx, req.Header)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, rest.ErrRefused) {
			return nil, fmt.Errorf("a2a: posting to %s: %w", receiver, err)
		}
		return nil, &rest.TransportError{Receiver: receiver, Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, &rest.StatusError{Receiver: receiver, Code: resp.StatusCode}
	}
	if mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		return nil, fmt.Errorf("a2a: %s answered with content type %s", receiver, logsafe.Quote(resp.Header.Get("Content-Type")))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, &rest.TransportError{Receiver: receiver, Err: err}
	}
	if len(raw) > maxBody {
		return nil, fmt.Errorf("a2a: the response from %s is over %d bytes", receiver, maxBody)
	}
	return parseResponse(receiver, raw)
}

// parseResponse extracts the response envelope from a JSON-RPC reply.
func parseResponse(receiver string, raw []byte) ([]byte, error) {
	if err := strictjson.Valid(raw); err != nil {
		return nil, fmt.Errorf("a2a: %s answered with invalid JSON", receiver)
	}
	var r rpcResponse
	if err := strictjson.UnmarshalKnown(raw, &r); err != nil || r.JSONRPC != "2.0" || string(r.ID) != `"1"` {
		return nil, fmt.Errorf("a2a: %s answered with no JSON-RPC response to this request", receiver)
	}
	if r.Error != nil {
		var d struct {
			Status int `json:"status"`
		}
		if len(r.Error.Data) > 0 && strictjson.UnmarshalKnown(r.Error.Data, &d) == nil && d.Status >= 400 && d.Status <= 599 {
			return nil, &rest.StatusError{Receiver: receiver, Code: d.Status}
		}
		if r.Error.Code == errCodeUnavailable {
			return nil, &rest.StatusError{Receiver: receiver, Code: http.StatusServiceUnavailable}
		}
		return nil, fmt.Errorf("a2a: %s answered JSON-RPC error %d", receiver, r.Error.Code)
	}
	var probe struct {
		Kind string `json:"kind"`
	}
	if err := strictjson.UnmarshalKnown(r.Result, &probe); err != nil {
		return nil, fmt.Errorf("a2a: %s answered with a malformed result", receiver)
	}
	var parts []Part
	switch probe.Kind {
	case "message":
		var m Message
		if err := strictjson.UnmarshalKnown(r.Result, &m); err != nil {
			return nil, fmt.Errorf("a2a: %s answered with a malformed message", receiver)
		}
		if len(m.Parts) == 0 {
			return nil, rest.ErrNoResponse
		}
		parts = m.Parts
	case "task":
		var t Task
		if err := strictjson.UnmarshalKnown(r.Result, &t); err != nil || t.Status.State != stateCompleted {
			return nil, fmt.Errorf("a2a: %s answered with a task that isn't completed", receiver)
		}
		for _, a := range t.Artifacts {
			parts = append(parts, a.Parts...)
		}
	default:
		return nil, fmt.Errorf("a2a: %s answered with a result of kind %s", receiver, logsafe.Quote(probe.Kind))
	}
	data, err := onlyData(parts)
	if err != nil {
		return nil, fmt.Errorf("a2a: %s's answer: %w", receiver, err)
	}
	return data, nil
}
