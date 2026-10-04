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
- **Canonical form is the signature.** Envelope signing uses the
  Python-compatible canonical JSON in `pkg/internal/pyjson`. Any change to
  canonicalization or signed fields is a protocol change: golden vectors must
  be regenerated deliberately, never edited to make a test pass.

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

Scaffolding only: repository, tooling, CI and the design of record. Next is
milestone 1 in [`docs/DESIGN.md`](./docs/DESIGN.md#a14-build-milestones):
`pkg/internal/{sig,pyjson}` and `pkg/envelope`, tested against golden vectors
from the Python reference implementation. Per-package coverage floors
(`verify-coverage`) join the sweep with that first package.
