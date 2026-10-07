# AGENTS.md — supercargo

Instructions for AI agents working in this repository.

## Start here

[`README.md`](./README.md) for what supercargo is, and
[`docs/DESIGN.md`](./docs/DESIGN.md) for the design this repository
implements: the demo scenario, the envelope and intents, the trust and
authorization rules, the LLM roles, and the build milestones. Read the design
before adding protocol surface, and keep it current in the same PR as the
change it describes.

## What this project is

A Go module, `github.com/go-steer/supercargo`, with two layers:

- `pkg/`: reusable ULP protocol pieces (envelope, signing, discovery,
  intents, delegation, transports). Treated as an SDK: its exported API is
  checked by `verify-apidiff`.
- `cmd/` + `internal/`: the demo: a ULP node per company (operator or
  shipper role), the shipper chat CLI, the presenter console, the shipment
  simulator, and the LLM adapters.

## Hard rules (violations are bugs)

- **No unverified message is ever processed.** Every inbound envelope goes
  through the full pipeline (resolve sender key → verify signature → replay
  and clock-skew check → validate body → authorize) before any handler runs.
  No debug flag, test hook or "trusted network" path skips a step in
  production code.
- **Authorization happens before data access and before any LLM call.**
  A verified identity is not an authorization. Load only the data the
  authorized request needs, and only then involve a model.
- **LLMs never hold keys, sign, or send on their own.** Models propose; deterministic code
  builds, signs and sends envelopes. Actions with external effect (sending a
  booking, granting access) require explicit human confirmation in the CLI,
  never a model's say-so.
- **Model output is untrusted input.** Validate it (JSON shape, cited event
  IDs belong to the authorized booking, length caps) and fall back to a
  deterministic template on failure.
- **Shipper-private context never leaves the shipper.** Business constraints
  (delivery deadlines, warehouse slots) are used by the shipper's own LLM and
  are never put in an envelope.
- **Verify the bytes you process.** Envelopes are JWS (`docs/DESIGN.md`
  §A.4): the signature covers the exact payload bytes, and handlers act on
  those same verified bytes. Never re-serialize before verifying or between
  verifying and acting. Unverified content stays behind `envelope.Parse`'s
  unverified type.
- **The key decides the algorithm.** A message's `alg` must equal the
  resolved key's `alg`; never let the sender choose. New algorithms plug in
  through `pkg/sig` (`Signer`/`Verifier` plus a JWK type). Any change to the
  JWS profile, the signed fields or an algorithm is a protocol change: golden
  vectors are regenerated deliberately, never edited to make a test pass.

## Build & test

```bash
dev/tools/ci                # full local presubmit sweep, fast-fail order
dev/ci/presubmits/build     # go build ./...
dev/ci/presubmits/test-unit # go test -race ./...
dev/ci/presubmits/vet       # go vet ./...
dev/ci/presubmits/lint-go   # golangci-lint (auto-installs the pinned version)
dev/ci/presubmits/verify-go-format  # gofmt + goimports (fix: dev/tools/fix-go-format)
dev/ci/presubmits/verify-mod-tidy   # go.mod/go.sum are tidy
dev/ci/presubmits/verify-apidiff    # exported-API diff vs the last release tag
dev/ci/presubmits/verify-vuln       # govulncheck
dev/ci/presubmits/smoke             # end-to-end scenarios in dev/smoke (real processes, sockets)
dev/ci/presubmits/verify-no-agent-attribution  # no agent credit in commits
```

The default test run needs **no network, no credentials and no LLM API key**.
Tests use the fake model (`internal/llm/fake`) and keys minted in-process. Do
not check private keys into the repo, not even test ones. Real-model runs are
opt-in and never part of `dev/tools/ci`.

## Conventions

- Mirror `purser` / `core-agent` conventions (package layout, `dev/`
  tooling); a maintainer of one repo should recognize the others.
- **License headers everywhere.** The Apache 2.0 boilerplate attributed to
  Google LLC sits atop every Go / shell / YAML source file. Markdown carries no
  header. `goheader` enforces it on `.go`; run `dev/tools/add-license-headers`
  for the rest.
- **Conventional Commits**, small self-contained commits, bodies explaining
  *why* + the verification done. **DCO sign-off** (`git commit -s`).
- **No agent attribution of any kind**: no `Co-Authored-By` trailer for an
  agent, no "Generated with" footer, no agent author identity, in commits, PR
  titles/bodies, or any committed artifact. Enforced by the required
  `agent attribution` check. See [`CONTRIBUTING.md`](./CONTRIBUTING.md).
- **Tests before merging.** Every new package ships unit tests; a bug fix
  ships a regression test. Security-relevant code (verification, replay,
  authorization, delegation) needs a negative test for every rejection path,
  asserting that the call *failed*.

## How we develop

Single long-lived branch `main`; short-lived `feat/…` `fix/…` `chore/…`
`docs/…` branches → PR → merge once the required CI checks are green. Rebase,
don't merge; `--force-with-lease` on your own branch is fine, never force-push
`main`. Full contributor flow + DCO in [`CONTRIBUTING.md`](./CONTRIBUTING.md).

Conventions worth knowing at agent prompt time:

- **Run presubmits before every push.** `dev/tools/ci` runs the same scripts CI
  runs (`dev/ci/presubmits/*`). A green local run is the green remote run.
- **Adversarial-review gate before every PR.** Before `gh pr create` on any
  change touching Go code: run a skeptic subagent over the staged diff
  (correctness, races, verification bypasses, authorization gaps, prompt
  injection paths — verified against real dependency source, not memory), fix
  or pin every finding, and record the outcome in the PR body under an
  `## Adversarial review` heading. For bug fixes, additionally **verify the new
  regression test FAILS on the pre-fix code**. Enforced by the `review-gate`
  **required** CI check (Go-touching PRs fail without the section; docs-only
  and bot PRs exempt). Optionally copy `dev/claude/settings-review-gate.json`
  into your local, untracked `.claude/settings.json`.
- **Admin merge protocol.** `gh pr merge <N> --admin --squash --delete-branch`
  is the maintainer path once CI is green. The adversarial gate + green CI
  *are* the review on this single-maintainer repo. Never merge unverified CI.
- **`[Unreleased]` grows on every merged PR.** Any user-visible change adds a
  bullet under `## [Unreleased]` in `CHANGELOG.md`.
- **Harness settings are not committed.** `.claude/` (and a personal
  `CLAUDE.md`) are git-ignored; the only checked-in Claude Code config is the
  opt-in sample under `dev/claude/`.

## Current state

Milestones 1 to 3 of [`docs/DESIGN.md`](./docs/DESIGN.md#a14-build-milestones)
are in:
- `pkg/envelope`, `pkg/sig`, `pkg/internal/strictjson`: JWS envelopes with
  pluggable signatures (Ed25519), checked against `testdata/golden/envelope.json`
  (made by joserfc via `dev/tools/gen-golden-vectors`; regenerate, never edit).
- `pkg/discovery`, `pkg/intents`, `pkg/internal/netguard`: profiles and the
  key cache, intent bodies, and the outbound guard every request to a host
  named by someone else goes through.
- `internal/node` (the pipeline), `pkg/transport/rest`, `internal/store`,
  `internal/operator`, `internal/shipper`: two nodes book over REST and TLS.
- `internal/config`, `internal/app`, `cmd/ulpnode` (`serve`, `keygen`,
  `book`): a node from its YAML config.
- The push path and hello ULP: `tnt.event`/`tnt.alert`, the operator's
  publisher and shipment simulator, the shipper's inbound handler, the
  control socket (`internal/control`) that `ulpnode book` and
  `ulpctl disrupt` use, and `dev/demo/hello-ulp`.
- `dev/smoke/*` (run by `dev/tools/smoke`): one end-to-end scenario per
  script, real processes over sockets, sharing `lib.sh` with `dev/demo`.
  A new demo-able scenario gets a smoke script here.

- The inquiry (milestone 4, part 1): `tnt.inquiry` over REST, `internal/llm`
  (the `Model` interface and the fake), the operator's `InquiryService`
  (authorize, then load one booking, then the model, then validate, else
  the template), and `ulpnode ask`.

- The A2A binding (milestone 4, part 2): `pkg/transport/a2a`, the agent
  card, and `node.ExchangeA2A` (falls back to REST).

- Claude and agent skills (milestone 4, part 3): `internal/llm/anthropic`,
  `internal/skills` (built-in `shipment-inquiry`, company skills from
  `skills.dir`). Milestone 4 is complete.

- The node API (#15): `internal/activity` (the activity stream, which the
  log renders from), the control API's reads, and `ulpnode watch`.

Next is milestone 5: `ulpchat` (the shipper assistant with tools) and the
alert summarizer, as a full-screen TUI; the UI decisions and mockups are in
issues #29 (M5) and #30 (M9, the browser UI), deployment in #38 and
telemetry in #49. Cited cause IDs in alerts must be checked against the
shipper's log before an alert reaches a model.
