# Golden vectors

`envelope.json` holds ULP envelopes (JWS, flattened JSON serialization,
Ed25519; see `docs/DESIGN.md` §A.4) made by
[joserfc](https://jose.authlib.org/), an off-the-shelf Python JOSE library. A
Go implementation that reads them correctly interoperates with any
conforming JOSE implementation, not just with itself.

| Field | Meaning |
|---|---|
| `keys` | The public JWKs that signed the vectors (`cy-2026-1`, and `cy-2026-2` as the "other" key) |
| `vectors[].wire` | The envelope exactly as sent |
| `vectors[].payload` | For valid vectors, the payload text that was signed: `Verify` must return exactly these bytes |
| `vectors[].error` | `null` (valid), `"signature"` (well formed, but not signed by the named key) or `"malformed"` (rejected by `Parse`) |

The invalid vectors cover the usual JWS attacks: tampered payload and `kid`,
another key, the deprecated `EdDSA` name, `HS256` keyed with the public key,
`alg: none`, the wrong `typ`, general and compact serializations, an
unprotected header, padded base64, and payloads that are signed but break
the envelope's shape.

Only public keys are recorded. The private keys exist only while the
generator runs.

**Never edit this file.** Changing the JWS profile, the signed fields or an
algorithm is a protocol change. Regenerate deliberately, and expect every
vector to change, since the keys are fresh:

```bash
uv run --no-project --with joserfc dev/tools/gen-golden-vectors
```

The reverse direction (Go seals, joserfc verifies) is opt-in, because it
needs Python:

```bash
SUPERCARGO_GO_SIGNED_OUT=/tmp/go-sealed.json \
  go test -run TestGoSealedEnvelopesForPython ./pkg/envelope/
uv run --no-project --with joserfc dev/tools/gen-golden-vectors verify-go /tmp/go-sealed.json
```
