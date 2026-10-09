# Agent runtime: where the assistants run (proposal)

**Status:** proposal for review (2026-10-09). Nothing here is built yet. It
changes where some of [DESIGN §A.10](./DESIGN.md#a10-llm-integration)'s
guardrails live, so it needs a decision before any code.

The question: should the assistants (the shipper's, and the carrier's in
#99) keep running on supercargo's own tool loop inside the node, or run as
a **separate agent process** on an agent framework, reaching the node's
tools over MCP?

## Where we are

- **Our own loop.** `internal/assistant` runs the turn: one `Generate` call
  per round. Each proposed tool call is checked (`llm.ValidateCall`, then
  the tool's own rules) and then run or refused. Anything with an external
  effect is a draft that only a person's confirmation sends. DESIGN §A.10
  explains why we didn't use an agent runner: a runner that executes the
  model's tool calls itself doesn't fit "models propose, code acts".
- **Two placements already.** `ulpchat` runs the assistant in its **own
  process** and reaches the node only through the control socket
  (`assistant.ControlNode`); keys, stores and signing stay in the node. The
  browser UI runs the same assistant **inside the node** for convenience
  (`control.InProcess`), through the same control-API calls. The boundary
  is logical there, not physical.
- **MCP is already planned** (DESIGN §A.7, #39–#42): the node's tools for
  the company's own agents, Gemini Enterprise among them, and a read-only
  on-ramp for partners.

## What changed

- **mast is a library.** `mast.RunWorkload(ctx, cfg, bundle, specs, input)`
  embeds the same governance its daemon runs: the agent loop on ADK v2, a
  write gate with durable human approval (`RequestConfirmation`, which
  survives restarts), an effect outbox (a replayed mutating call isn't
  repeated), budget ceilings, a behavioral watchdog, and durable sessions.
  Its gating is ADK v2 runner plugins, where a refusal or a plugin error
  stops the tool.
- **mast's extension point is MCP, not Go.** The public `Config` takes a
  model, a session service, a logger and budgets. A workload's tools come
  from MCP servers named in its bundle (plus a few built-ins). Its loop,
  permission gate, watchdog and providers are `internal/`.
- So running on mast means **serving our tools over MCP**, which is the
  separate-process shape anyway.

## Options

| Option | What it is | Verdict |
|---|---|---|
| A. Keep the in-node loop | Today's design; add an internal plugin seam (ordered before/after tool and model hooks, first refusal wins, fail closed) to name the existing checks | Safe and small; no "built on ADK" story |
| B. ADK's runner in-process | Our checks as `BeforeToolCallback`s | **No.** Hooks see the arguments only after ADK decoded them (malformed JSON already `{}`, duplicate keys merged), so "validate the bytes you process" is lost. ADK runs a response's calls concurrently by default. Sessions can't roll back a failed turn. No Claude adapter we can import |
| C. **Separate agent process over MCP** | The node serves the assistant's tools as an MCP server; an agent process (embedding mast, or Gemini Enterprise, or our loop) calls them | **Proposed**, in phases, with A kept as the default until C matches it |

## Proposal (option C)

```
  agent process: ulpagent (mast library) | Gemini Enterprise | ulpchat (our loop)
        │  MCP over the control socket, or loopback with a token
        ▼
  ulpnode: tool server ── node-side checks ── control API ── stores, keys, signing, pipeline
```

### Why

- **A hard boundary.** The agent process never holds keys, never sees a
  store, never touches the inbound pipeline: it can only call tools, and
  the node checks every call. That is the hard rules made physical.
- **The bytes are back.** MCP arguments reach the node as JSON bytes, so the
  node validates them exactly as `llm.ValidateCall` does now (one object,
  exact names, no case-variant duplicates, the schema) before decoding.
- **Swappable agents.** mast (on ADK v2), Gemini Enterprise, Claude, or our
  own loop, all against one tool surface with one set of checks.
- **Governance we'd otherwise build.** Durable approvals, the outbox, budgets
  and the watchdog come with mast; manual approval of inquiry answers has a
  natural home there.

### Where each guardrail lives

| Guardrail (today, in our loop) | Under option C |
|---|---|
| `llm.ValidateCall` (exact tool name, one JSON object, no case-variant duplicates, schema) | **Node**, on the MCP arguments' bytes |
| Our bookings only; the carrier side's records only | **Node** (it is authorization, and the node knows the records) |
| No private dates or notes in a question to a carrier | **Node** (it holds the business context), as well as the outbound business guard it already has |
| Outbound guard, profile verification, size caps on results | **Node**, unchanged |
| No tool with an external effect: drafts, sent only after a person confirms | **Node**: tools still only draft. The confirmation is either our dialog (the node keeps the drafts and the arming delay) or mast's write gate (an approval the node verifies before sending; see open questions) |
| **The counterparty rule** (a company the user named, by domain or known name) | **Changes.** The node can't see the user's words. Proposed: reaching a company that is new to this session needs a person's yes (a mast approval or our dialog). Alternative: the agent host passes the user's messages as trusted context. See open questions |
| At most 8 calls per response, 6 model calls per turn | **Agent side**: mast's budgets and watchdog. The node adds per-session rate limits as a backstop |
| Refusals go back to the model as error results | MCP tool errors, unchanged in spirit |
| Model output validated before use (inquiry answers, alert summaries) | **Unchanged**: those two model roles stay inside the node, with no tools |
| Model-call reporting (activity, OTel) | Agent side (mast's spans) plus the node's own record of every tool call |

### Phases

1. **The node serves the assistant's tools over MCP** (#40/#41). The same
   tool implementations, with the node-side checks above, over the control
   socket first (local, owner-only), later loopback or IAP with a token.
   This alone unlocks Gemini Enterprise and other MCP clients.
2. **`ulpagent`**: a small process that embeds mast (`RunWorkload`) and is
   pointed at that MCP server. The shipper's assistant first; the browser
   chat stays on the in-node loop.
3. **Compare and decide.** Same scenes, same tests (`dev/smoke/browser`, the
   assistant tests re-pointed at the MCP tools), same real models. If mast
   matches, the browser chat moves to `ulpagent`, and the in-node loop stays
   for `ulpchat` and as the fallback.

## Open questions

1. **The counterparty rule.** Approval for a company new to the session, or
   the user's messages as trusted context from the agent host? Approval is
   simpler and holds even if the agent is compromised; context keeps
   today's "just type the name" flow.
2. **Whose confirmation sends a booking.** Our dialog (node-side drafts, the
   arming delay, exactly what is sent) or mast's approval (durable, in
   mast-web)? If mast's, the node must check that a person approved that
   exact draft, not merely that the agent says so.
3. **Models.** mast brings its own Gemini and Claude providers (direct and on
   Vertex), internal to it; our Vertex wiring would be unused on that path.
   Providers are moving to go-steer/core-models, which has neither yet.
4. **Authentication of the MCP endpoint** beyond the control socket: a
   per-agent token, IAP on Cloud Run, or mTLS.
5. **The carrier's assistant** (#99): the same move, after the shipper's.

## Not in scope

- The inquiry responder and the alert summarizer stay in the node: they
  have no tools, and their output is validated there (DESIGN §A.10).
- Nothing here changes the protocol: envelopes, intents, discovery and the
  pipeline are untouched.
