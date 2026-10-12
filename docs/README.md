# supercargo docs

| Where | What it's for |
|---|---|
| [`DESIGN.md`](DESIGN.md) | **The design** this repository implements: the scenario, the envelope and intents, identity and trust, authorization, the LLM roles, and the milestones. Code comments cite its sections (§A.x), so it stays at the top. Keep it current in the same PR as the change it describes. |
| [`guides/`](guides/) | **How to do something** with what's built. |
| [`proposals/`](proposals/) | **Designs not yet decided or built**: written for review before any code, and folded into `DESIGN.md` once accepted. |
| [`reference/`](reference/) | **Background**: how the domain works outside this repository, for context when designing. Not a spec for what we build. |
| [`ui/`](ui/) | **UI design notes and mockups.** |

## Guides

- [Run the demo on Cloud Run](guides/runbook-cloudrun.md): from an empty project to Gemini Enterprise drafting a booking over MCP that a person confirms in the browser.
- [Present the hosted demo](guides/demo-script.md): the scenes in order, what to type, what to point at, and what to do when something goes wrong.

## Reference

- [Multi-leg freight shipping and API orchestration](reference/multi-leg-freight-and-api-orchestration.md): Incoterms and who books, freight forwarders and control towers, the standards bodies, and legacy EDI next to JSON APIs.

## UI

- [Route map](ui/route-map.md): the journey view's design.
- [Web UI mockup](ui/web-ui-mockup.html): the approved look of the browser UI (open it in a browser).
