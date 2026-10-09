#!/usr/bin/env bash
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Shared by the smoke tests in this directory and by dev/demo: build the
# binaries, mint keys, write the two companies' configs, start and stop
# nodes. Source it; it defines functions and sets nothing else.
#
# The nodes run in insecure-dev mode joined by a peer map (TLS is covered by
# the integration tests in internal/node). Keys are minted with
# `ulpnode keygen`; nothing is read from the repo but the source.

# demo_build DIR: build ulpnode and ulpctl into DIR/bin.
demo_build() {
  local dir=$1 root
  root="$(git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse --show-toplevel)"
  mkdir -p "$dir/bin"
  (cd "$root" && go build -o "$dir/bin/ulpnode" ./cmd/ulpnode && go build -o "$dir/bin/ulpctl" ./cmd/ulpctl && go build -o "$dir/bin/ulpchat" ./cmd/ulpchat)
}

# demo_ports: pick an operator and a shipper port (OP_PORT, SH_PORT), below
# the Linux ephemeral range (32768+) so an outgoing connection can't
# already hold one.
demo_ports() {
  local base=$((10000 + RANDOM % 10000))
  OP_PORT=$base
  SH_PORT=$((base + 1))
}

# demo_setup DIR HOURS_PER_SECOND: keys, configs and a booking request in
# DIR, using OP_PORT and SH_PORT. DIR is made owner-only: the signing keys
# and the control sockets (DIR/ob.sock, DIR/cy.sock) live there.
# web_hosts PORT prints the web section's hosts line for the Cloud
# Workstations proxy's name for PORT, when running on a workstation
# ($WEB_HOST is set there); nothing otherwise.
web_hosts() {
  [[ -n "${WEB_HOST:-}" ]] && printf '\n  hosts: ["%s-%s"]   # the Cloud Workstations proxy' "$1" "$WEB_HOST"
  return 0
}

# web_origin PORT prints the origin a browser reaches node PORT's UI at
# for the presenter page: the Cloud Workstations proxy's, or 127.0.0.1
# (one site for every port, so each framed UI keeps its session; the
# *.localhost names are a site each).
web_origin() {
  if [[ -n "${WEB_HOST:-}" ]]; then printf 'https://%s-%s' "$1" "$WEB_HOST"; else printf 'http://127.0.0.1:%s' "$1"; fi
}

# web_presenter prints the operator's presenter section and both nodes'
# frameAncestors line (the presenter page is on the operator's node).
web_frame_ancestors() {
  printf '\n  frameAncestors: ["%s"]   # the presenter page' "$(web_origin "$OP_PORT")"
}
web_presenter() {
  printf '\n  presenter:               # %s/ui/presenter: both UIs side by side' "$(web_origin "$OP_PORT")"
  printf '\n    panels:'
  printf '\n      - { name: "Cymbal Shoes · shipper", url: "%s/ui/" }' "$(web_origin "$SH_PORT")"
  printf '\n      - { name: "Ocean Blue · carrier", url: "%s/ui/" }' "$(web_origin "$OP_PORT")"
}

demo_setup() {
  local dir=$1 hps=$2 root llm
  root="$(git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse --show-toplevel)"
  # ULP_LLM picks the models (every model role: Cymbal's assistant and
  # summarizer, Ocean Blue's inquiry responder); the default is the fake
  # model, which needs nothing. ULP_LLM_MODEL overrides the model ID.
  #   anthropic       Claude by API key (ANTHROPIC_API_KEY)
  #   gemini          Gemini by API key (GEMINI_API_KEY)
  #   vertex-claude   Claude on Vertex AI (ANTHROPIC_VERTEX_PROJECT_ID, CLOUD_ML_REGION)
  #   vertex-gemini   Gemini on Vertex AI (GOOGLE_CLOUD_PROJECT, GOOGLE_CLOUD_LOCATION)
  # Vertex AI uses Application Default Credentials (a gcloud login): no key.
  local model="${ULP_LLM_MODEL:-}" gem="${ULP_LLM_MODEL:-gemini-3.8-flash}"
  case "${ULP_LLM:-fake}" in
    fake) llm="{ provider: fake }   # deterministic answers, no API key" ;;
    anthropic) llm="{ provider: anthropic${model:+, model: $model} }   # the key comes from ANTHROPIC_API_KEY" ;;
    gemini) llm="{ provider: gemini, model: $gem }   # the key comes from GEMINI_API_KEY" ;;
    vertex-claude) llm="{ provider: anthropic, backend: vertex${model:+, model: $model} }   # Vertex AI: Application Default Credentials" ;;
    vertex-gemini) llm="{ provider: gemini, backend: vertex, model: $gem }   # Vertex AI: Application Default Credentials" ;;
    *) echo "demo_setup: ULP_LLM must be fake, anthropic, gemini, vertex-claude or vertex-gemini" >&2; return 1 ;;
  esac
  # ULP_OTEL_ENDPOINT=localhost:4318 exports both nodes' traces there
  # (dev/demo/otel up starts a local viewer).
  local telemetry=""
  if [[ -n "${ULP_OTEL_ENDPOINT:-}" ]]; then
    [[ $ULP_OTEL_ENDPOINT =~ ^[A-Za-z0-9.:/_-]+$ ]] || { echo "demo_setup: ULP_OTEL_ENDPOINT must be host:port or an http URL" >&2; return 1; }
    # Both nodes export to the same viewer, so each continues the other's
    # traces: one trace from Cymbal's ask through Ocean Blue's answer.
    local metrics=""
    if [[ -n "${ULP_OTEL_METRICS_ENDPOINT:-}" ]]; then
      [[ $ULP_OTEL_METRICS_ENDPOINT =~ ^[A-Za-z0-9.:/_-]+$ ]] || { echo "demo_setup: ULP_OTEL_METRICS_ENDPOINT must be host:port or an http URL" >&2; return 1; }
      metrics=", metricsEndpoint: \"$ULP_OTEL_METRICS_ENDPOINT\", metricInterval: 10s"
    fi
    telemetry="telemetry: { endpoint: \"$ULP_OTEL_ENDPOINT\", insecure: true, trustPeers: [ocean-blue.example, cymbal-shoes.example]$metrics }"
  fi
  chmod 700 "$dir"
  [[ -f "$dir/ob.pem" ]] || "$dir/bin/ulpnode" keygen --kid ob-demo-1 --out "$dir/ob.pem" >/dev/null
  [[ -f "$dir/cy.pem" ]] || "$dir/bin/ulpnode" keygen --kid cy-demo-1 --out "$dir/cy.pem" >/dev/null

  local peers="  ocean-blue.example: 127.0.0.1:$OP_PORT
  cymbal-shoes.example: 127.0.0.1:$SH_PORT"

  cat >"$dir/operator.yaml" <<EOF
domain: ocean-blue.example
name: Ocean Blue
role: operator
listen: 127.0.0.1:$OP_PORT
tls: { mode: insecure-dev }
keys:
  - kid: ob-demo-1
    privateKeyFile: $dir/ob.pem
peers:
$peers
partners: [cymbal-shoes.example]
operator:
  refPrefix: OB-BKG
  containerPrefix: OCBU
  vessel: Ocean Blue Aurora
  voyage: 42W
  departureHour: 18
  transit: 134h
simulator:
  hoursPerSecond: $hps
llm: $llm
skills:
  dir: "$root/dev/demo/skills/ocean-blue"   # Ocean Blue's own agent skills
control:
  socket: $dir/ob.sock
web:                     # the browser UI at http://ocean-blue.localhost:$OP_PORT/ui/
  auth: demo             # no password: local demos only
  personas:
    - { id: sam, name: Sam, title: "Ops desk", roles: [operator] }$(web_hosts $OP_PORT)$(web_frame_ancestors)$(web_presenter)
$telemetry
EOF

  cat >"$dir/shipper.yaml" <<EOF
domain: cymbal-shoes.example
name: Cymbal Shoes
role: shipper
listen: 127.0.0.1:$SH_PORT
tls: { mode: insecure-dev }
keys:
  - kid: cy-demo-1
    privateKeyFile: $dir/cy.pem
peers:
$peers
partners: [ocean-blue.example]
control:
  socket: $dir/cy.sock
business:
  file: $dir/business.yaml   # private: never in an envelope
mcp:
  server:                  # Cymbal's tools for its own agents (Gemini CLI, Gemini Enterprise) at /mcp
    tokenFile: $dir/mcp-token
    uiURL: "$(web_origin "$SH_PORT")/ui/"   # where a person confirms an agent's drafts
addressBook:
  file: $dir/address-book.yaml   # the partners Cymbal knows
directory:
  file: $dir/directory.yaml      # where Cymbal searches for new ones
llm: $llm
web:                     # the browser UI at http://cymbal-shoes.localhost:$SH_PORT/ui/
  auth: demo             # no password: local demos only
  personas:
    - { id: alex, name: Alex, title: "Logistics", roles: [shipper] }$(web_hosts $SH_PORT)$(web_frame_ancestors)
$telemetry
EOF

  # The MCP endpoint's bearer token (kept between runs), and a Gemini CLI
  # project that uses it: cd "$dir/gemini" && gemini.
  if [[ ! -s "$dir/mcp-token" ]]; then
    (umask 077 && head -c 36 /dev/urandom | base64 | tr -d '/+=\n' >"$dir/mcp-token")
  fi
  chmod 600 "$dir/mcp-token"
  mkdir -p "$dir/gemini/.gemini"
  cat >"$dir/gemini/.gemini/settings.json" <<EOF
{
  "mcpServers": {
    "cymbal-shoes": {
      "httpUrl": "http://127.0.0.1:$SH_PORT/mcp",
      "headers": { "Authorization": "Bearer $(cat "$dir/mcp-token")" }
    }
  }
}
EOF
  chmod 600 "$dir/gemini/.gemini/settings.json"

  # Cymbal's private business context: what the delay will mean to it.
  cat >"$dir/business.yaml" <<'EOF'
shipments:
  - shipperRef: CY-PO-55120
    requiredBy: 2026-10-15
    receivingSlot: { date: 2026-10-15, window: "08:00-12:00", warehouse: "Cymbal Newark DC, dock 4" }
    notes: "Spring launch stock: shelves must be full by Friday the 16th"
notes:
  - "Tell the launch team about any delay that puts the receiving slot at risk"
EOF

  # The partners Cymbal knows. Ocean Blue isn't one yet: Cymbal finds it
  # in the directory.
  cat >"$dir/address-book.yaml" <<'EOF'
entries:
  - domain: cymbal-footwear.example
    name: Cymbal Footwear
    roles: [consignee]
    description: Our US retail arm; receives at the Newark DC.
EOF

  # A directory to search (Google Cloud API hub in a deployment). Entries
  # are each company's own claim: only Ocean Blue's domain has a profile
  # that verifies, so North Star Lines shows up as unverified.
  cat >"$dir/directory.yaml" <<'EOF'
entries:
  - domain: ocean-blue.example
    name: Ocean Blue
    roles: [carrier]
    modes: [OCEAN]
    lanes: [{ from: NLRTM, to: USEWR }, { from: USEWR, to: NLRTM }]
    description: Transatlantic container service, weekly departures from Rotterdam.
  - domain: north-star.example
    name: North Star Lines
    roles: [carrier]
    modes: [OCEAN]
    lanes: [{ from: NLRTM, to: USEWR }, { from: DEHAM, to: USNYC }]
    description: North Atlantic container lines.
  - domain: cymbal-shoes.example
    name: Cymbal Shoes
    roles: [shipper]
EOF

  cat >"$dir/booking.json" <<'EOF'
{
  "shipper_reference": "CY-PO-55120",
  "shipper_name": "Cymbal Shoes",
  "port_of_loading": "NLRTM",
  "port_of_discharge": "USEWR",
  "containers": [{ "equipment_size_type": "45G1", "count": 1, "gross_weight_kg": 12000 }],
  "cargo": { "description": "Footwear (sneakers)", "hs_code": "6404" },
  "earliest_departure": "2026-10-06",
  "subscription": {
    "endpoint": "http://cymbal-shoes.example/ulp",
    "intents":  ["ulp.tnt.event", "ulp.tnt.alert"]
  }
}
EOF
}

# smoke_start DIR: start both nodes in the background, logging JSON to
# DIR/operator.log and DIR/shipper.log, and wait until both answer. Their
# PIDs are appended to SMOKE_PIDS.
SMOKE_PIDS=()
smoke_start() {
  local dir=$1
  "$dir/bin/ulpnode" serve --config "$dir/operator.yaml" --log-format json 2>"$dir/operator.log" &
  SMOKE_PIDS+=($!)
  "$dir/bin/ulpnode" serve --config "$dir/shipper.yaml" --log-format json 2>"$dir/shipper.log" &
  SMOKE_PIDS+=($!)
  smoke_wait_up "$dir" "$OP_PORT" "${SMOKE_PIDS[-2]}"
  smoke_wait_up "$dir" "$SH_PORT" "${SMOKE_PIDS[-1]}"
}

smoke_wait_up() {
  local dir=$1 port=$2 pid=$3
  for _ in $(seq 1 100); do
    if curl -fsS "http://127.0.0.1:$port/.well-known/ulp" >/dev/null 2>&1; then return 0; fi
    if ! kill -0 "$pid" 2>/dev/null; then
      smoke_fail "$dir" "the node for port $port exited (port taken?)"
    fi
    sleep 0.1
  done
  smoke_fail "$dir" "the node on port $port didn't come up"
}

# smoke_stop: stop the nodes started by smoke_start.
smoke_stop() {
  local p
  for p in "${SMOKE_PIDS[@]:-}"; do kill "$p" 2>/dev/null || true; done
  wait 2>/dev/null || true
  SMOKE_PIDS=()
}

# smoke_fail DIR MESSAGE: print MESSAGE and the logs, and exit 1.
smoke_fail() {
  local dir=$1; shift
  echo "smoke: $*" >&2
  local f
  for f in "$dir"/*.log; do
    [[ -f "$f" ]] || continue
    echo "--- $(basename "$f")" >&2
    cat "$f" >&2
  done
  exit 1
}

# smoke_wait_for DIR FILE PATTERN SECONDS: wait until FILE has a line
# matching PATTERN (grep -E), or fail.
smoke_wait_for() {
  local dir=$1 file=$2 pattern=$3 secs=$4 i
  for ((i = 0; i < secs * 10; i++)); do
    if grep -qE "$pattern" "$file" 2>/dev/null; then return 0; fi
    sleep 0.1
  done
  smoke_fail "$dir" "no line matching $pattern in $(basename "$file") after ${secs}s"
}
