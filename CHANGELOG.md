# Changelog

All notable changes to supercargo are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Entries under **Changed** and **Removed** that touch `pkg/` are the release
notes an SDK consumer reads before upgrading. An incompatible change to the
exported API is recorded here *and* acknowledged in
[`dev/api-breaks.txt`](./dev/api-breaks.txt) — see "Changing the exported API"
in [`CONTRIBUTING.md`](./CONTRIBUTING.md).

## [Unreleased]

### Added

- Model and tool spans (#45), after OpenTelemetry's GenAI conventions: each model call is a `chat <model>` span with the provider, model, token counts, finish reason, attempt, prompt hash, skills and the caller's verdict on the reply (ok, invalid with a reason, error); each assistant tool call is an `execute_tool` span. The inquiry's model calls nest under its handler (with a `template fallback` event), the alert summarizer has its own span with the cause check, and the assistant's turns are spans. No prompt or reply is ever recorded.
- Trace export (#48): a `telemetry:` config section (OTLP over HTTP or gRPC, endpoint, sampling, service name) or the standard `OTEL_EXPORTER_OTLP_*` variables; off when neither is set. `dev/demo/otel up` starts a local Jaeger, and `ULP_OTEL_ENDPOINT=localhost:4318` makes the demo's nodes export to it.
- OpenTelemetry traces in the node (#44): each inbound message is one trace (`ulp.ingress`) with a span per pipeline step (parse, verify and key resolution, replay, decode, authorize with its decision, handle, reply seal); each outbound exchange is a `ulp.egress` client span with the response's checks under it. Spans carry the envelope's facts and the decision, never a body. Nothing is recorded until telemetry is configured (export comes with #48).
- The shipper assistant's tool loop (#17), `internal/assistant`: the engine behind `ulpchat`'s chat. Its tools are `get_operator_profile`, `draft_booking` (a draft only; there is no send tool), `list_shipments`, `get_events` and `ask_operator`. Each call is validated before it runs and reaches only companies the user named and bookings the node holds. There is a round limit, and each model and tool call is reported. The built-in `freight-booking` and `shipment-tracking` skills are included, and a fake model plays the demo's scenes with no API key.
- The alert summarizer (#20): each alert the shipper receives becomes a notification for its own staff, on the activity stream and in the log (`alert summary`). The cited cause events are checked against the shipper's log before anything reaches the model, the impact on the receiving slot or required-by date is computed, and the model's reply is validated (citations, length, plain text) with a template fallback. The shipper uses `llm:` too now, with the built-in `alert-summary` skill. In the demo, Cymbal learns that the 72-hour delay misses its receiving slot.
- The shipper's private business context (#19): `business: { file: … }` holds per-shipment `requiredBy` dates, receiving slots and notes, loaded strictly. The node's new `OutboundGuard` refuses any outbound body carrying its text, so it never leaves the shipper. The demo's Cymbal has one (the 15th receiving slot).
- Tool calling (#16): `internal/llm` requests carry tool definitions and history with calls and results; responses carry the model's tool calls and stop reason. `llm.ValidateCall` checks a call against its tool's schema before anything runs (exact names, one object, no duplicate members, the schema subset our tools use). The Anthropic adapter maps tools, `tool_use` and `tool_result` blocks. `fake.Turns` scripts conversations, and `fake.AdversarialCalls` are calls a loop must refuse.
- The node API (#15): each node's **activity stream**, one structured entry per envelope decision, record, model call, voyage and dropped push, served as Server-Sent Events at `GET /events` on the control socket (with replay and resume). The JSON log is now rendered from the same entries. The shipper serves `GET /shipments`, `GET /shipments/{operator}/{ref}/events` and `GET /profile/{domain}`; the operator serves `GET /bookings` and `GET /bookings/{ref}/events`. `ulpnode watch` follows a stream. `discovery.Document.KIDs` is new in `pkg/`. `dev/smoke/api` covers it.
- Claude and agent skills (milestone 4, part 3):
  - `internal/llm/anthropic`: the Anthropic adapter, derived from mast's provider. JSON replies come through a forced `respond` tool.
  - `llm: { provider: anthropic }` takes the key from `ANTHROPIC_API_KEY` (or `apiKeyEnv`); a node without it doesn't start.
  - `internal/skills`: a `SKILL.md` loader with limits. The built-in `shipment-inquiry` skill is embedded; company skills come from `skills.dir`, and a broken one stops the node. The inquiry responder's skills go after its rules, and each model call logs them by name and hash.
  - Ocean Blue's `delay-phrasing` skill in the demo. `ULP_LLM=anthropic` runs the demo against Claude, and an opt-in real-model test (`SUPERCARGO_REAL_LLM=1`) sits outside CI.
- The A2A binding (milestone 4, part 2), `pkg/transport/a2a`: JSON-RPC `message/send` carrying the signed envelope in a data part, and the agent card at `/.well-known/agent-card.json`. Operators publish `transports.a2a` and the `shipment-inquiry` skill, and the shipper's inquiries go over A2A, falling back to REST for a receiver without an endpoint. Refusals before verification come back as JSON-RPC errors carrying the bare status. Node log lines name the binding (`via`).
- The inquiry, Scene 5 over REST (milestone 4, part 1):
  - `ulp.tnt.inquiry` and `ulp.tnt.inquiry.response` bodies in `pkg/intents`.
  - `internal/llm`: a small provider-neutral `Model` interface, and a fake model with adversarial replies.
  - The operator's `InquiryService`. Only the booking's shipper may ask. Only that booking's record and records reach the model, with the question in its own block. The reply must be one JSON object whose answer passes the free-text rules and whose citations are this booking's. Otherwise it retries once, then falls back to a deterministic template. Each model call is logged (prompt hash, tokens, latency, outcome).
  - `ulpnode ask`: asks through the running shipper node, and marks the cited records the shipper holds signed.
  - Limits on inquiries in progress (2 per partner, 8 in all).
  - `rest.ClientOptions.MaxTimeout`: `Timeout` now bounds an exchange whose context has no deadline, and a longer context deadline is honoured up to `MaxTimeout`.
  - `llm: { provider: fake }` answers grounded in the records with no API key; without an `llm` section every answer is the template.
  - `dev/smoke/inquiry`, and step 5 of `dev/demo/hello-ulp`.
- Hello ULP, milestone 3's demo: `dev/demo/hello-ulp` sets up an operator and a shipper on localhost and prints the commands for four terminals (or opens them in tmux). You book, watch the voyage's events arrive signed and verified, then disrupt the voyage and watch the delay alert arrive.
  - The operator's shipment simulator: each confirmed booking plays out on an accelerated clock (`simulator.hoursPerSecond`): gate-in, load, departure, arrival estimate, arrival, discharge, gate-out.
  - `ulpctl disrupt`: delays a voyage. The simulator publishes a new arrival estimate and a delay alert citing it.
  - A local control socket per node (`control.socket`): HTTP over a Unix socket in an owner-only directory, used by `ulpnode book` and `ulpctl`.
  - The shipper's log shows each event and alert it recorded; the operator's shows each one it published.
- Smoke tests now live in `dev/smoke/`, one script per scenario, sharing helpers with the demo: `booking`, and `push` (the demo, scripted). `dev/tools/smoke` runs them all.
- The push path for track and trace:
  - `ulp.tnt.event` and `ulp.tnt.alert` bodies in `pkg/intents`.
  - `node.Push` and `rest.Client.PostTo`, which post to a subscriber's registered endpoint on its own domain, through the guarded client.
  - The operator's `Publisher`: records each push (one per ID per booking), then delivers it in order per subscription, with idle workers exiting and a cap on active ones; it retries only failures another attempt can fix, and each retry is re-signed. `rest.TransportError` marks failures to exchange HTTP at all.
  - The shipper's `Inbound` handler: only the booking's operator, for a subscribed intent; each event recorded once.
  - An event log.
- The operator's booking store is capped (`store.MaxBookings`), since anyone may request a booking; a request past the cap is answered `rejected` (`at_capacity`).
- `cmd/ulpnode`: `serve` runs a node from its YAML config, with every HTTP server timeout set and the peer map logged at startup. `keygen` mints an Ed25519 signing key (mode 0600, never overwriting) and prints its public JWK. `book` sends a booking after an explicit `y`.
- `internal/config` (strict YAML: unknown keys are errors; the signing key must be owner-only) and `internal/app` (a node assembled from config, with the same peers and TLS policy for discovery and the REST client, and partners pinned in the key cache).
- `dev/tools/smoke`, now in the CI sweep: two real `ulpnode` processes booking over sockets. Milestone 2 is complete.
- `pkg/transport/rest`: the REST binding. `Handler` serves `POST /ulp` (size-capped while reading) for any `Processor`, and `Client` posts to the endpoint in the receiver's profile, always through the guarded outbound client.
- The ULP node (`internal/node`): its profile, the inbound pipeline (parse → receiver and intent → key → verify → skew → replay → body → authorize → handle → signed reply, with nothing signed in answer to an unverified message: refusals before verification are bare HTTP statuses, after it signed `ulp.error` replies with fixed details), `Exchange` for outbound requests with full response checks, and a structured event per decision.
- The operator's booking service and the shipper's `Book`, with in-memory replay and booking stores. Two nodes now book over REST and TLS, which is milestone 2's demo.
- `pkg/discovery`: party profiles at `/.well-known/ulp`.
  - `Fetcher` connects only to public addresses (or an explicit static peer map), follows no redirects and validates the profile strictly.
  - `Cache` resolves (domain, kid) to a verifier with a TTL, a per-domain refresh floor, single-flight fetches and a cap on concurrent fetches. It never serves an expired profile. Unknown domains can't evict a valid profile, and pinned partner domains are never evicted or starved.
- `pkg/intents`: typed, validated bodies for `booking.request`, `booking.confirmation` and `ulp.error`, plus the per-intent header rules. `Decode` matches member names exactly, so a case variant can't mean one thing here and another elsewhere.
- `envelope.ValidDomain`, the party-identity rule, for other packages.
- `pkg/envelope`: the signed ULP envelope, a JWS (RFC 7515, flattened JSON
  serialization) with `typ` `ulp-envelope+jws`. `Seal` signs it. `Parse`
  decodes strictly and returns an `Unverified` message that exposes what
  key resolution and logging need, never the body. `Unverified.Verify` is
  the only way to the `Envelope`, and it requires the key's algorithm to
  match the message's. `NewHeader` and `Header.Reply` build conversation
  headers.
- `pkg/sig`: pluggable `Signer`/`Verifier` interfaces, each bound to one JOSE
  algorithm, with Ed25519 (`alg` `Ed25519`, RFC 9864) as the first.
  Public keys are JWKs, parsed through an `Algorithms` set that new
  algorithms extend. Ed25519 keys that are non-canonical or of small order
  are refused.
- Golden vectors made with joserfc, an independent Python JOSE library
  (`dev/tools/gen-golden-vectors`), including the usual JWS attacks as
  negative cases.
- `verify-coverage`: per-package statement-coverage floors in
  `dev/coverage-floors.txt`, now part of `dev/tools/ci`.

- Repository scaffolding: Apache 2.0 license, the `dev/` presubmit tooling
  and CI workflows shared with the other go-steer repositories, and the
  design of record in `docs/DESIGN.md`.

### Fixed

- `ulpnode book` sends through the running shipper node (its control socket). It used to book from a node of its own that exited with the command, so the running node never had the booking and refused every push for it.
