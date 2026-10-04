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

- Repository scaffolding: Apache 2.0 license, the `dev/` presubmit tooling
  and CI workflows shared with the other go-steer repositories, and the
  design of record in `docs/DESIGN.md`.
