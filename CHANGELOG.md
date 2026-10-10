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

- The browser UI's activity marks each envelope entry with the binding it went over: an `A2A` badge (questions to the carrier) or `REST` (everything else), with a tooltip saying the signed envelope is the same either way.

- Voyage-wide disruptions: `ulpctl disrupt --voyage 42W [--vessel …]`, the control API's `POST /disrupt` with `voyage`, and a "this booking or the whole voyage" choice in the carrier's Disrupt dialog. One decision delays every booking under way on the voyage (arrived ones are left out), each with its own estimate and signed alert; a booking that can't take the delay is reported and the rest go ahead, and one delayed whose alert couldn't be sent is marked `applied`, with a warning not to disrupt again.

- `hello-ulp` on a Cloud Workstation says to open each company's address once before the presenter page (the proxy signs you in per address, and its sign-in page won't load in a frame).

- Partners and the directory, MVP1 step 6 (#79): the assistant lists the partners we know (`list_partners`: the company's address book, `addressBook.file`, and the carriers we've booked with) and searches a directory for new ones (`find_partners`, `directory.file`; Google Cloud API hub next), verifying each candidate's profile at its domain; a directory entry is never trust. People can name partners by name: a partner's name in the user's own words counts as naming its domain, for address-book partners and verified finds, and an ambiguous name is asked about. The booking dialog (browser and terminal) marks a carrier we've never booked with and don't list as a new partner. The demo gets an address book and a directory.

- The assistant's model calls are in the node's activity stream and log, like the other model roles': each `model` entry now names its agent (inquiry responder, alert summarizer, or `assistant · <persona>` from the browser) and the tools the reply asked for. Never the conversation's text or the tools' arguments: the stream is everyone's on that side.

- The browser assistant shows its work and formats its answers: while a turn runs, each tool call appears as it starts and finishes, with its arguments in short (`GET /ui/api/assistant/progress`, which hands out no drafts); replies render a safe Markdown subset (paragraphs, lists including loose and one-level nested ones, headings, code blocks, bold, italics, code) as elements and text only, never markup from the model; the browser smoke test checks the renderer. The assistant gains `OnToolStart`.

- Gemini, and Vertex AI for both providers: `internal/llm/gemini` (on `google.golang.org/genai`; Gemini 3's thought signatures round-trip with tool calls), and `llm.backend: vertex` for Gemini and Claude, authenticated by Application Default Credentials (no key; project and region from the usual environment variables, names configurable). `ULP_LLM` in the demo and smoke scripts takes `gemini`, `vertex-gemini` and `vertex-claude`, with `ULP_LLM_MODEL`. The Anthropic adapter asks for its `respond` tool instead of forcing it (Claude on Vertex AI refuses a forced tool) and takes a JSON object in text. An opt-in real-model test, `TestRealModels`, covers all three paths.

- MVP1 step 5's UI (#81): a **progress strip** on every shipment and booking card, drawn from the journey (legs done, in progress, delayed; at sea as a hollow estimate; each mark linking to its message), and the **presenter page** (`/ui/presenter`): each company's own UI in a frame, side by side, each signed in on its own node. New `web` settings: `frameAncestors` and `presenter`. `hello-ulp` sets them up and prints the presenter's address; the browser smoke test checks the strip's stages and the presenter's frames.

- The journey model, MVP1 step 5's foundation (#81, #83): each booking's journey, built from the records the node holds (legs with planned, estimated or actual times and their source events; ports with events, an unscheduled call splitting a leg; the position, at a port or at sea as an estimate; the alerts), served at `GET /shipments/{operator}/{ref}/journey` (shipper) and `GET /bookings/{ref}/journey` (operator), and through the browser UI's side-filtered API. The ports table gains approximate coordinates, for the route map (MVP2).

- A browser smoke test, step 4 of MVP1 (#81): `dev/smoke/browser` runs the demo in headless Chrome against two real nodes (book through the assistant and its dialog, disrupt from the carrier view, the delay arrives live). The driver (`chromedp`) is its own module under `dev/smoke/browserdrive`, so the SDK's `go.mod` doesn't carry it. CI requires the test; locally it skips without Chrome (`SUPERCARGO_CHROME` points at one).

- The browser UI acts, step 3 of MVP1 (#81): the shipper's **assistant** in the browser (the same Go assistant as `ulpchat`, one per session, behind an aurora-framed panel), booking through a **confirmation dialog** that shows every field sent and the deadline that isn't, armed 1.5 s after the node handed the draft to the page (the node refuses an earlier send), each draft sent at most once; and **Disrupt voyage…** for the carrier, behind its own dialog. Delays stand out without borrowing red (which means a trust failure): a filled orange HIGH badge, a banner with what the delay means for the shipper from its own summary ("misses your deadline"), a glow when an alert arrives live, and a DELAYED shipment card with the old ETA struck through. The page polls when the live stream is unavailable. `app.ShipperAssistant` builds the assistant for both `ulpchat` and the browser; `control.InProcess` serves a control API client in process.

- The browser UI, step 1 of MVP1 (#81, #30): each node serves its own people a UI under `/ui/` on its listener, in the approved mockup's look (grayscale themes, colour-coded messages, activity as folded JSON with tooltips). People sign in as a persona from the node's new `web` section (`auth: demo`: buttons, no password, so the node must listen on loopback only), and the node cuts every read to the persona's roles before it reaches the control API. The UI answers only to the host names it's served as (localhost names, the listen address, and `web.hosts`, such as a Cloud Workstations proxy name), so a rebinding web page can't use it. The messages and activity panels are live. `hello-ulp` gives Ocean Blue and Cymbal Shoes a persona each and prints their UI addresses: `http://ocean-blue.localhost:8441/ui/` and `http://cymbal-shoes.localhost:8442/ui/`, or the workstation proxy's on a Cloud Workstation.

- The message log (#30, #75): each node keeps the envelopes it sent and received, exactly as on the wire (the JWS as text), with the signed header and verified body (messages refused before their signature verified keep only the claimed header and up to 4 KiB of wire text, and may fill at most a fifth of the log, so a flood can't push out real traffic), the outcome, and the side it belongs to. It serves them on the control API at `GET /messages` and `GET /messages/{seq}`, filtered by side, conversation and sequence number. Every activity entry now carries its side too (`operator` or `customer`), and `GET /events?side=` filters the stream; the browser UI will show one tab per role. The bytes never reach the activity stream or the log.

- Nodes with several roles (#75): `roles: [operator, shipper]` (the one-role `role:` still works), and a `receiver` role, the consignee on others' bookings, which takes their events and can't book. Each side's records are kept apart (a carrier is never answered from its customer-side bookings), each model reads one role's data, every role's control routes share one socket, and startup fails if two roles claim one intent or route. A node now refuses any message from its own domain, and never sends one to itself (`node.ErrSelf`). The business file's outbound guard covers what the customer side sends, never a carrier role's replies, which echo their request. In `pkg/`: `discovery.RoleReceiver`; profiles may list several roles, each once. Nodes built before this refuse a profile that lists `receiver`.

- Discovery extras from the ULP payload specification (#67, tracking #68): profiles may carry `capabilities` (reverse-DNS names with versions, an informative `spec_url` and tags such as `ulp:dcsa:track-and-trace` and `ulp:transport:rest`), `supported_dialects` and `transports.mcp`, each strictly validated. Nodes publish capabilities for the intents they accept and every dialect they decode; the shipper's profile view (and the assistant's `get_operator_profile`) shows them. In `pkg/`: `discovery.Capability`, `Profile.Capabilities`, `Profile.SupportedDialects`, `Transports.MCP`, `Document.Capability`, `Document.SupportsDialect`, `MaxCapabilities`, `MaxTags` and `MaxDialects`, and `intents.DialectFor` and `intents.Dialects`.

- The structured status pull (#65): `ulp.ocean.tnt.request` / `ulp.ocean.tnt.response` (the standard payloads, DCSA Track & Trace status with dialect `DCSA_TNT 2.2.0`). Only the booking's shipper may ask, and no model is involved: the operator reports the DCSA status, vessel, ports, current location, ETA and events, and the shipper marks the events it never received as pushes. `ulpnode status`, `POST /status` on the control API, and the assistant's `get_status` tool. `smoke/push` checks it.

### Fixed

- Gemini 3 replies with a short limit (the alert summary's) stopped at the token limit before answering, so the template answered instead: Gemini counts its thinking against the output limit. The adapter now gives thinking its own budget (1024 tokens by default) on top of the reply's limit, and counts thought tokens as output in usage.

- The browser UI dropped a refresh asked for while one was in flight, so a delay alert arriving milliseconds after its event could never show (no alert, banner or DELAYED card until the next message), and overlapping refreshes of the left panel could land out of order. Each loader now runs one fetch at a time and fetches again for anything that arrived meanwhile. The browser smoke test holds a fetch across the disruption to check it.

### Security

- Go 1.26.9 (from 1.26.6) for the standard library's `net/http`, `html/template` and related advisories that govulncheck found reachable, and `golang.org/x/net` v0.60.0.

### Changed

- **Protocol change: the eBL dialect is `DCSA_OCEAN_CONTAINER` `2.0.0`** (was `0.0.1`), following the ULP payload schemas' update, and the protocol version is now `0.4-demo` (envelopes and profiles). A booking confirmation's header `dialect` must say `2.0.0`; the version bump makes a mixed pair refuse the booking request itself, before a carrier books anything the shipper would then refuse. Golden vectors regenerated, and a test now ties them to the code's version and dialect. pf's new `native_standard` and `modality` body members are deliberately not adopted (DESIGN §A.5): our signed header's `dialect` already says both.

- **Protocol change: the booking in the ULP payload schema** (#66, tracking #68). The intents are now `ulp.ocean.booking.request` and `ulp.ocean.booking.confirmation`. The request carries pf's shipper-side fields in snake_case (`shipper_reference`, `shipper_name`, `port_of_loading` and `port_of_discharge` as UN/LOCODEs, `containers` with `equipment_size_type`, `count` and `gross_weight_kg`, `cargo.hs_code`, `earliest_departure`, an optional `service_contract_number`). A confirmed booking carries pf's eBL (`ebl`: transport document reference, vessel, voyage, ports, parties, containers with numbers, seals, weights and reefer set points, and a status), under dialect `DCSA_OCEAN_CONTAINER` (now `2.0.0`). The status pull now asks about the eBL's transport document reference, and `ulpnode status` names both references. The shipper refuses a confirmation whose eBL contradicts its request (ports, parties, service contract, status, or containers by size-type and weight) and records nothing. An operator's `refPrefix` must now be 1-40 of `[A-Za-z0-9-]`, so its references always validate. In `pkg/`: `intents.BookingRequestBody`, `Equipment`, `Cargo` and `BookingConfirmationBody` changed; `intents.EBL`, `ShippingContainer`, `DialectDCSAEBL` and `MaxGrossWeightKg` are new; `intents.Vessel` is gone (`BookingConfirmationBody.Vessel()`, `EquipmentRefs()` and `TransportDocumentReference()` read the eBL). The assistant's `draft_booking` takes port codes and a gross weight. Golden vectors regenerated.
- **Protocol change: track & trace events and alerts in the ULP payload schema** (#64, tracking #68). `ulp.tnt.event` carries pf's DCSA Track & Trace event (`event_id`, `event_type` now including `SHIPMENT`, `event_classifier_code`, `event_date_time`, `event_type_code`, `description`, `un_location_code`, `facility_name`, `vessel_name`, `voyage_number`, `equipment_reference`), and `ulp.tnt.alert` uses snake_case (`alert_id`, `previous_eta`, `new_eta`, `cause_event_ids`). The envelope header gains an optional `dialect` (`standard`, `version`, `schema_url`), required as `DCSA_TNT 2.2.0` on events and refused on extension intents. In `pkg/`: `envelope.Header.Dialect` and `envelope.Dialect`, `intents.HeaderFor`, `intents.SetDialect` and `intents.DialectDCSATnT` are new; `intents.TnTEventBody`'s fields changed and `intents.Transport` is gone. The protocol version is now `0.3-demo` (envelopes and profiles), so a mixed-version pair fails plainly. Golden vectors regenerated, with a dialect-bearing event and its malformed variants.

### Added

- Metrics (#47): envelopes by direction, intent, binding, outcome, refusal code and partner; pipeline latency; model call latency, tokens and verdicts (GenAI conventions); alert summaries; records; publisher drops, retries and queue depth; voyages in progress. Labels are low-cardinality by construction. They export with the traces, or to `telemetry.metricsEndpoint`; `dev/demo/otel up` now starts Prometheus too.
- Traces across nodes (#46): an exchange's trace context travels as W3C `traceparent` in the HTTP request, never in the envelope. A partner's context is linked to, not continued, unless `telemetry.trustPeers` names the partner (the demo does, so an inquiry is one trace across both companies). `rest.ClientOptions.Header` is new in `pkg/`: a hook that adds request headers from the context.
- Model and tool spans (#45), after OpenTelemetry's GenAI conventions: each model call is a `chat <model>` span with the provider, model, token counts, finish reason, attempt, prompt hash, skills and the caller's verdict on the reply (ok, invalid, error); each assistant tool call is an `execute_tool` span. The inquiry's model calls nest under its handler (with a `template fallback` event), the alert summarizer has its own span with the cause check, and the assistant's turns are spans. No prompt or reply is ever recorded.
- Trace export (#48): a `telemetry:` config section (OTLP over HTTP or gRPC, endpoint, sampling, service name) or the standard `OTEL_EXPORTER_OTLP_*` variables; off when neither is set. `dev/demo/otel up` starts a local Jaeger, and `ULP_OTEL_ENDPOINT=localhost:4318` makes the demo's nodes export to it.
- OpenTelemetry traces in the node (#44): each inbound message is one trace (`ulp.ingress`) with a span per pipeline step (parse, verify and key resolution, replay, decode, authorize with its decision, handle, reply seal); each outbound exchange is a `ulp.egress` client span with the response's checks under it. Spans carry the envelope's facts and the decision, never a body. Nothing is recorded until telemetry is configured (export comes with #48).
- Pretty node logs (#21): `ulpnode serve --log-format pretty` renders the same records as the JSON log as one line each for people: direction, intent, peer, binding and what the node decided (`VERIFIED (kid …)  ALLOWED`, `DENIED`, `REJECTED`), events and alerts, model calls and alert summaries in the aurora. Colour comes from the shared theme (`--log-theme dark|light`), only on a terminal and never with `NO_COLOR`. `dev/demo/hello-ulp`'s log panes use it; JSON stays the default for scripts and the smoke tests.
- The confirmation dialog (#18): the only way a booking leaves the shipper. A draft the assistant makes opens a dialog in `ulpchat` that shows everything the carrier will learn and the deadline that stays home; only a `y` typed into it sends (`Assistant.Send`), and the assistant then reports the outcome. Declined drafts stay open (`/send BD-0001`).
- `ulpchat`, the shipper's chat (#13): a full-screen Bubble Tea TUI on Charm's v2 releases, over the shipper assistant and the node's control socket. The transcript is styled as a log, in mast-web's grayscale themes (dark or light, following the terminal or `--theme`), with the Gemini aurora on the assistant's rows. `internal/ui/theme` holds the colour tokens for the TUI, the pretty logs and the browser UI (as CSS custom properties). The composer is copied from go-steer/core-tui, with provenance.
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
