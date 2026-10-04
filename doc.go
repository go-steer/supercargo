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

// Package supercargo is the root of a Go implementation of ULP, the
// Universal Logistics Protocol: agent-to-agent booking and track & trace
// between companies, using signed, intent-named envelopes over A2A.
//
// The reusable protocol pieces live under pkg/; the demo nodes and agents
// live under cmd/ and internal/. See docs/DESIGN.md for the design of
// record and the build milestones.
package supercargo
