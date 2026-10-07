# dev/

Build- and test-tooling. The same scripts power both local development and
GitHub Actions CI, so a green local run is the same green run as remote.

## Quickstart

```bash
# Run every CI check locally (fast-fail order).
dev/tools/ci

# Run all checks even after a failure (collect every problem at once).
dev/tools/ci --keep-going

# Auto-fix formatting (gofmt + goimports).
dev/tools/fix-go-format
```

Missing tools (`golangci-lint`, `goimports`, `govulncheck`, `apidiff`)
auto-install into `$GOBIN` (or `$(go env GOPATH)/bin`) on first use. No setup
needed beyond a Go toolchain.

## Layout

```
dev/
├── api-breaks.txt         # acknowledged incompatible API changes
├── coverage-floors.txt    # per-package statement-coverage floors
├── claude/                # opt-in Claude Code config sample (not live config)
├── tools/                 # entry points you run locally
│   ├── ci                 # aggregator — runs every check below
│   ├── verify-go-format   # gofmt -s + goimports check (read-only)
│   ├── fix-go-format      # gofmt -s -w + goimports -w (auto-fix)
│   ├── vet                # go vet ./...
│   ├── build              # go build ./...
│   ├── lint-go            # golangci-lint (auto-installs v2.12.1)
│   ├── verify-mod-tidy    # `go mod tidy` clean check
│   ├── verify-apidiff     # exported-API diff vs the last release tag
│   ├── test-unit          # go test -race -coverprofile
│   ├── verify-coverage    # per-package floors vs coverage.out
│   ├── smoke              # runs every scenario in dev/smoke (or those named)
│   ├── verify-vuln        # govulncheck ./...
│   ├── verify-no-agent-attribution # no agent credit in commits/PR text
│   ├── add-license-headers # bulk-applier for the Apache 2.0 header
│   ├── gen-golden-vectors # Python: regenerate testdata/golden (not in ci)
│   ├── common.sh          # shared bash helpers (ensure_tool, run_step)
│   └── .golangci.yml      # linter config
├── smoke/                 # end-to-end scenarios: real ulpnode processes
│   ├── lib.sh             # shared with dev/demo: build, keys, configs, start/stop
│   ├── booking            # two nodes book over REST; a declined booking isn't sent
│   ├── push               # hello ULP scripted: events, ulpctl disrupt, the alert
│   └── inquiry            # Scene 5: ulpnode ask, a grounded, signed answer
├── demo/
│   └── hello-ulp          # the interactive milestone 3 demo (see below)
└── ci/
    └── presubmits/        # thin delegators called by .github/workflows/
        ├── verify-go-format
        ├── vet
        ├── build
        ├── lint-go
        ├── verify-mod-tidy
        ├── verify-apidiff
        ├── verify-vuln
        ├── verify-no-agent-attribution
        ├── verify-coverage
        ├── smoke
        └── test-unit
```

## Adding a check

1. Drop a new script under `dev/tools/<name>` (executable, `set -euo pipefail`,
   sources `common.sh`).
2. Add it to the `STEPS` array in `dev/tools/ci`.
3. Add a one-line delegator under `dev/ci/presubmits/<name>` that
   `exec`s the tool script.

That's it. The `ci` workflow runs `dev/tools/ci`, so a new step is picked up
without touching any YAML — the delegator exists so the step can also be run
on its own, and so nothing in a workflow ever has to know what a check does.

## CI on PRs

`.github/workflows/ci.yml` runs the whole sweep in one `ci` job on every push
to `main` and every PR. No path filters, deliberately: a required status check
on a path-filtered workflow never reports on a PR the filter excludes, and
GitHub parks that PR on "Expected — Waiting for status" forever. The Go
pipeline here is seconds, so docs-only PRs simply run it too.

`apidiff` is its own workflow rather than a step in that job, for two reasons:
it needs `fetch-depth: 0` to see the baseline tag, which the main job should
not pay for; and it is deliberately **not** a required check. It depends on a
tag being fetchable, so a mirror hiccup or a moved tag would turn into a red
required check on unrelated PRs. Its verdict is visible on every PR; promote
it to required once it has a release cycle of clean history behind it.

`ci.yml` sets `SUPERCARGO_CI_SKIP=apidiff` so the aggregator omits the step that
runs in that separate job. The step list itself stays in one place —
`dev/tools/ci` — so the local sweep cannot drift from the remote one.

Branch protection on `main` requires:

- `ci`
- `review-gate`
- `agent attribution`

## agent attribution

`.github/workflows/agent-attribution.yml` scans every commit on a PR branch
(message lines plus author and committer identities) and the PR title and
body for agent attribution: an agent co-author trailer, a "Generated with"
footer, an agent identity. It runs the **base branch's** copy of
`dev/tools/verify-no-agent-attribution`, so a PR cannot switch the check off
by editing it. Locally, `dev/tools/ci` runs the scanner's self-test and scans
commits not yet on `origin/main`. Ported from
[core-agent](https://github.com/go-steer/core-agent), which also carries a
post-merge backstop on `main`; add that here if a commit ever lands on `main`
without passing the PR check.

## Coverage floors

`dev/tools/verify-coverage` holds every package in `coverage.out` to its
floor in `dev/coverage-floors.txt` (ported from purser). A package with no
entry fails, so nothing arrives unmeasured; lowering a floor is a reviewable
diff hunk. It regenerates a stale profile itself.

## Golden vectors

`dev/tools/gen-golden-vectors` (Python; run it with
`uv run --no-project --with joserfc`) regenerates
`testdata/golden/envelope.json` with an independent JOSE library. Its
`verify-go` mode checks Go-sealed envelopes with that library. Neither runs
in `dev/tools/ci`, whose default run needs no Python.
See [`testdata/golden/README.md`](../testdata/golden/README.md).

## Smoke tests

Each executable in `dev/smoke/` is one end-to-end scenario: it builds
`ulpnode` and `ulpctl`, mints signing keys with `ulpnode keygen` into a temp
dir, starts an operator and a shipper as separate processes on localhost
(insecure-dev mode, joined by a peer map, control sockets in the temp dir),
drives them through the CLIs, and checks their JSON logs. `dev/tools/smoke`
runs them all (`dev/tools/smoke push` runs one); CI runs them through
`dev/tools/ci`. They need `curl`. TLS is exercised by the integration tests
in `internal/node`.

- `booking`: a booking confirmed over REST, verified both ways; one answered
  with anything but `y` is never sent.
- `push`: the hello ULP demo, scripted. The voyage's events reach the
  shipper, verified and recorded; `ulpctl disrupt` produces a delay alert
  that reaches it too.
- `inquiry`: Scene 5. After a disruption, `ulpnode ask` gets an answer from
  the operator's fake model, citing records the shipper holds signed.

To add one, write an executable script in `dev/smoke/` that sources
`lib.sh` (`demo_build`, `demo_ports`, `demo_setup`, `smoke_start`,
`smoke_wait_for`, `smoke_fail`) and prints a `✓` line on success. A scenario
someone may want to watch belongs in `dev/demo/` too, on the same helpers.

## The interactive demo

`dev/demo/hello-ulp [--tmux] [DIR]` sets up milestone 3's demo, Ocean Blue
and Cymbal Shoes on localhost, and prints the command for each of four
terminals: the two nodes' logs, booking, and `ulpctl disrupt` (or opens
them in tmux). Ports and the clock speed come from `OP_PORT`, `SH_PORT` and
`HOURS_PER_SECOND`. In tmux, mouse mode is on for the demo session (click a pane to switch, scroll with the wheel, drag borders), and `HELLO_ULP_SESSION` names the session: tests should use their own, plus a separate server (`TMUX_TMPDIR`), so they never replace a running demo. `ULP_LLM=anthropic` has Ocean Blue answer inquiries with
Claude (the key from `ANTHROPIC_API_KEY` in the operator's terminal); the
default, the fake model, needs no key.

## review-gate

`.github/workflows/review-gate.yml` fails any PR that touches `.go` files
whose body has no "Adversarial review" section. It is the tool-agnostic
backstop behind the convention in [`AGENTS.md`](../AGENTS.md) "How we develop";
`dev/claude/settings-review-gate.json` is an opt-in local hook that catches the
same thing before CI ever sees it. Docs-only and bot PRs are exempt.

## License headers

Every source file carries the full Apache 2.0 header at the top, attributed to
Google LLC:

```
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
```

(`#`-prefixed for shell, YAML, and Python.) The `goheader` linter inside
`dev/tools/lint-go` enforces this on every `.go` file — CI fails if a new Go
source is missing it. For shell, YAML, and Python files, run
`dev/tools/add-license-headers` after creating new ones; the script is
idempotent and normalizes any existing header to the current canonical form.

Its `HEADER_BODY` and the `goheader` template in `dev/tools/.golangci.yml` are
two copies of the same text and drift silently. Change one, change the other,
and run `dev/tools/lint-go` to confirm they still agree.

## Pinned tool versions

| Tool          | Version    | Source                                                     |
|---------------|------------|------------------------------------------------------------|
| golangci-lint | v2.12.1    | `dev/tools/lint-go` (`GOLANGCI_LINT_VERSION`)              |
| apidiff       | pinned     | `dev/tools/verify-apidiff` (`APIDIFF_VERSION`)             |
| goimports     | latest     | `dev/tools/fix-go-format`, `dev/tools/verify-go-format`    |
| govulncheck   | latest     | `dev/tools/verify-vuln`                                    |

`apidiff` is pinned to a pseudo-version because `golang.org/x/exp` carries no
tags. It is pinned at all — unlike govulncheck — because its classification of
a change as compatible or incompatible *is* the gate; letting it float would
let an upstream reclassification turn a green PR red with no local diff to
explain it.

Bump deliberately — new linter releases can introduce findings that block CI.
When you bump golangci-lint, run `dev/tools/lint-go` locally first to fix
anything new before pushing.
