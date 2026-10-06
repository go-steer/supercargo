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

// Derived from go-steer/mast@603b826132aaef26b1615809ebe5e7154d6b4b3f
// (pkg/a2a/wire.go), cut down to the subset ULP's binding uses. A data
// part's data is kept as raw JSON: it carries a signed envelope, which is
// passed on as it arrived.

package a2a

import (
	"encoding/json"
	"fmt"
)

// WellKnownCardPath is the spec-defined agent-card discovery path.
const WellKnownCardPath = "/.well-known/agent-card.json"

// ProtocolVersion is the A2A spec line this binding implements.
const ProtocolVersion = "0.3.0"

// TransportJSONRPC is the card's transport identifier for JSON-RPC 2.0
// over HTTP, the only transport this binding speaks.
const TransportJSONRPC = "JSONRPC"

// AgentCard is the discovery document served at WellKnownCardPath. It
// lists a party's agentic skills only: structured intents are served by
// the REST binding (docs/DESIGN.md, A.3).
type AgentCard struct {
	Name               string       `json:"name"`
	Description        string       `json:"description,omitempty"`
	URL                string       `json:"url"`
	Version            string       `json:"version,omitempty"`
	ProtocolVersion    string       `json:"protocolVersion,omitempty"`
	PreferredTransport string       `json:"preferredTransport,omitempty"`
	Capabilities       Capabilities `json:"capabilities"`
	DefaultInputModes  []string     `json:"defaultInputModes,omitempty"`
	DefaultOutputModes []string     `json:"defaultOutputModes,omitempty"`
	Skills             []AgentSkill `json:"skills"`
}

// Capabilities is the card's capability declaration.
type Capabilities struct {
	Streaming         bool `json:"streaming"`
	PushNotifications bool `json:"pushNotifications"`
}

// AgentSkill is one named capability. Per the v0.3 spec it carries media
// types, not schemas.
type AgentSkill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name,omitempty"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	InputModes  []string `json:"inputModes,omitempty"`
	OutputModes []string `json:"outputModes,omitempty"`
}

// Message is an A2A message (kind "message").
type Message struct {
	Kind      string `json:"kind"`
	MessageID string `json:"messageId"`
	Role      string `json:"role"`
	Parts     []Part `json:"parts"`
	TaskID    string `json:"taskId,omitempty"`
	ContextID string `json:"contextId,omitempty"`
}

// Part is a message or artifact part: Text for kind "text", Data for kind
// "data". Data stays raw JSON.
type Part struct {
	Kind string          `json:"kind"`
	Text string          `json:"text,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Task is an A2A task (kind "task"), the other shape a message/send result
// may take.
type Task struct {
	Kind      string     `json:"kind"`
	ID        string     `json:"id"`
	Status    TaskStatus `json:"status"`
	Artifacts []Artifact `json:"artifacts,omitempty"`
}

// TaskStatus is a task's lifecycle position.
type TaskStatus struct {
	State string `json:"state"`
}

// Artifact is a task output.
type Artifact struct {
	Parts []Part `json:"parts,omitempty"`
}

// Message roles and the task state this binding reads.
const (
	roleUser       = "user"
	roleAgent      = "agent"
	stateCompleted = "completed"
)

// The JSON-RPC method this binding serves.
const methodMessageSend = "message/send"

// JSON-RPC 2.0 error codes, with A2A's -32000 band.
const (
	errCodeParse          = -32700
	errCodeInvalidRequest = -32600
	errCodeMethodNotFound = -32601
	errCodeInvalidParams  = -32602
	errCodeInternal       = -32603
	errCodeUnavailable    = -32000 // retry later
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is a JSON-RPC 2.0 error object. For a refusal the ULP pipeline
// made before verifying the message, Data is {"status": N}: the bare HTTP
// status the REST binding would have answered with.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("a2a: JSON-RPC error %d: %s", e.Code, e.Message)
}

type messageSendParams struct {
	Message *Message `json:"message"`
}
