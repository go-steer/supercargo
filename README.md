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
milestones.

Milestones 1 and 2 are in:
- **Protocol:** signed JWS envelopes (`pkg/envelope`), pluggable signatures with Ed25519 first (`pkg/sig`), party profiles and a key cache (`pkg/discovery`), and intent bodies (`pkg/intents`).
- **Node:** the node pipeline and the REST binding (`internal/node`, `pkg/transport/rest`).
- **Binary:** `ulpnode` (`serve`, `keygen`, `book`).

Two companies' nodes now book over REST and TLS, each verifying the other's signed envelopes. Next is milestone 3: the shipment simulator, subscriptions and webhook push.

To try it, mint a key with `ulpnode keygen`, write a config ([§A.12](./docs/DESIGN.md#a12-configuration-per-company)), and run `ulpnode serve`. `dev/tools/smoke` does exactly that for an operator and a shipper on localhost.

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
