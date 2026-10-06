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
