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
