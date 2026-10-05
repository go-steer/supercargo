# supercargo

> *supercargo* (n.): a merchant's representative aboard a ship, responsible
> for the cargo and for doing business on the owner's behalf at each port.

**supercargo** is a Go implementation of **ULP**, the Universal Logistics
Protocol: agent-to-agent booking and track & trace between companies that
share no infrastructure. It has two parts:

- **A protocol SDK** (`pkg/`): signed, intent-named envelopes, discovery and
  published keys, REST and A2A bindings, and delegation.
- **A cross-company demo:** a shipper (Cymbal Shoes) and an ocean carrier
  (Ocean Blue), each running its own ULP node, with an LLM on each side in the
  roles where one helps. The shipper's assistant turns requests into protocol
  calls and alerts into business impact. The carrier's responder answers
  free-text shipment inquiries, but only after deterministic code has verified
  and authorized the request.

Neither LLM ever holds a key, signs a message, or sees data its caller isn't
authorized for.

## Status

Early. The design of record is [`docs/DESIGN.md`](./docs/DESIGN.md): the
demo scenario scene by scene, plus the technical design (envelope, intents,
trust and authorization, transport bindings, LLM integration) and the build
milestones. Nothing is implemented yet beyond the repository scaffolding.

## Development

Requires a Go toolchain (the version in `go.mod` downloads automatically).

```bash
dev/tools/ci          # every check CI runs, locally, fast-fail order
dev/tools/fix-go-format
```

Contributor flow, commit conventions and the review gate are in
[`CONTRIBUTING.md`](./CONTRIBUTING.md); tooling details in
[`dev/README.md`](./dev/README.md).

## License

[Apache 2.0](./LICENSE).
