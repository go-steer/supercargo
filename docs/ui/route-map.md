# Live route map (design)

**Status:** designed for MVP1, built in MVP2 (decided 2026-10-08; MVP1 #81, MVP2 #84; issue #83).

The map shows where a shipment is and how its journey is going, from the
signed messages the viewer's node actually holds. It is the browser UI's
answer to "where's my container?", the same question the assistant answers
in words.

## Principles

1. **Every mark on the map traces to a signed message the node holds.**
   A position, a completed leg, a delay all come from track & trace events
   and alerts this node received (or, for a carrier, published), never from
   a hardcoded route or a progress percentage. Clicking a mark opens that
   message in the messages panel: the map is a view of the evidence.
2. **Each party sees its own side.** The map is built by the node from its
   own records and cut to the persona's sides, like every other read. A
   shipper's map shows what its carrier told it; the carrier's shows what
   it published. Nobody has an all-seeing view; a cross-party overview is
   the presenter page putting each party's own map side by side. A real
   "control tower" is a party of its own that a shipper grants access to
   (delegation, M7), not a node that sees everything.
3. **Place names and coordinates come from the node's own table**, keyed
   by UN/LOCODE, never from a partner's text (the same rule as port names
   today, `internal/ports`).
4. **Estimates look like estimates.** An observed position (an actual
   event at a port) is a solid mark; a position between calls is drawn
   dashed and labelled "estimated", because no message says exactly where
   the vessel is at sea.
5. **No external fetches by default.** The basemap is bundled, so the UI
   keeps its strict Content Security Policy and works offline and behind
   the workstation proxy. Map imagery from Google Maps Platform is an
   option for hosted deployments, behind config.

## The journey model (built in MVP1)

The node derives a **journey** per booking from records it already keeps:

```
Journey
  booking, operator, shipper
  legs[]:
    from, to          UN/LOCODE (names and coordinates from the node's table)
    mode              ocean (road with the truck leg, #80)
    vessel, voyage
    departure         planned | estimated | actual  (time, and the event ID behind it)
    arrival           planned | estimated | actual  (time, and the event ID behind it)
    status            planned | in progress | done | delayed
  calls[]:            every port with an event: UN/LOCODE, event IDs, kind (load, depart, arrive, unscheduled)
  position:           { at: UN/LOCODE | between: [from, to], estimated: bool, since: event ID }
  alerts[]:           alert IDs with their leg, severity and new ETA
```

- **Legs** come from the booking (port of loading → port of discharge) and
  from ports off the plan where an *actual* event is held (the demo's
  Halifax): such an unscheduled call splits a leg in two. A plan or an
  estimate elsewhere doesn't redraw the route, and at most 16 such ports
  are drawn (the journey says when it left some out): the events are an
  operator's, and a hostile one could report thousands of places.
- **Status** comes from the events: done once anything actually happened
  at the leg's far end (an arrival, a load, a discharge), in progress after
  an actual departure, delayed when the latest arrival estimate is past the
  booked arrival (a delay alert's new ETA arrives as such an estimate).
- **Position** follows the legs, so the two never disagree: at sea on the
  first leg not done if it actually departed, else at that leg's port of
  departure; at the end once every leg is done.
- **Alert causes** are listed only if the node holds those events, as
  everywhere else alerts are shown.
- **At sea** is "between" two calls after an actual departure, marked
  estimated. No fraction of the way is
  given: the shipper can't know how far along a vessel is at sea (in the
  demo, the voyage even runs on the operator's simulated clock), so the
  views draw "at sea" without pretending to know where.
- Every field carries the event or alert IDs it came from, so the UI can
  link each mark to its message.

**API:** `GET /shipments/{operator}/{ref}/journey` (customer side) and
`GET /bookings/{ref}/journey` (operator side) on the control API, served to
the browser through the UI's side-filtered forwarding.

**In MVP1** the journey model, its API and the coordinates in the ports
table are built, and the **progress strip** (MVP1 step 5) is the journey's
first renderer:

```
OB-BKG-1042   NLRTM ●━━━━━━━━━━━━━━━◐┄┄┄┄┄┄┄┄┄┄○ USEWR      ETA Fri 16th (was Tue 13th) ⚠
              departed 7th 18:00     estimated at sea        delayed: engine repair, Halifax
```

## The map (built in MVP2)

```
┌ Route · OB-BKG-1042 ─────────────────────────────────── observed ● estimated ◌ ┐
│ [Full route] ◀ ▶ [① NLRTM → CAHAL · ocean] [② CAHAL → USEWR · ocean]  [Fit][World]│
│        ╭──────────── ocean · DCSA Track & Trace ───────╮                       │
│  NLRTM ●━━━━━━━━━━━━━━━━━━━━━━━◐ ┄┄┄┄┄ ◇ CAHAL ┄┄┄┄┄┄┄┄┄○ USEWR               │
│  Rotterdam   departed 7th 18:00 (EV-3)  unscheduled call   ETA 16th (EV-5) ⚠  │
│                                         (AL-6, high)                           │
│  [◀ prev call]  [next call ▶]     click any mark: its message opens on the right│
└────────────────────────────────────────────────────────────────────────────────┘
```

- **Basemap:** a bundled world outline (Natural Earth 1:110m land, public
  domain), drawn as SVG in the theme's greys; Web Mercator, framed to the
  journey's bounds.
- **Legs:** great-circle arcs coloured by mode (the category tokens: ocean
  cyan, road orange later); done legs solid, upcoming dashed, a delayed leg
  in yellow with its alert's pin.
- **Calls:** a numbered pin per port with events; the unscheduled call as a diamond.
- **Leg labels name the standard** behind the leg: the dialect of the signed messages that reported it ("DCSA Track & Trace" for ocean; DSDC with the truck leg, #80), so a multimodal journey shows its standards on the map.
- **Hop chips** across the top: the full route, then one chip per leg, with previous/next; selecting one frames it. **Fit route** and **World** reset the frame; zoom and pan with the wheel and drag.
- **The container:** a dot at its position, pulsing when an event moved it
  in the last minute; a hollow dashed dot when estimated.
- **Live:** the map redraws when the activity stream brings a record for
  the booking, as the shipments panel does now.
- **Where it shows:** a Route tab in each participant's shipment or booking
  view, and on the presenter page, one map per participant panel.
- **Interaction:** step from call to call, hover for times and sources,
  click to open the message; keyboard accessible.
- **Hosted option:** with `web.map: google` and a Maps JavaScript API key
  from Secret Manager, the basemap is Google's, with Map, Terrain and
  Satellite views; the CSP then allows Google's script and tile hosts, and
  nothing else changes. (Third-party tile servers, as pf's demo uses, are
  not an option: their terms and attribution vary, and every tile is a
  fetch the CSP would have to allow.)

## Compared with pf's demo map

pf's Control Tower map (October 2026) draws a global journey arc and
per-hop arcs coloured by mode, with hop chips, standard labels per hop and
imagery tiles. Its routes and coordinates are hardcoded and the map does
not move with tracking events; its control tower sees every party's
messages. Ours keeps the good presentation ideas (hop chips, standards on
legs, fit/world) and differs where it matters: the journey is the node's
evidence, live, and each party sees only its own.

## Out of scope here

- Live vessel positions from AIS: the map shows what the protocol says,
  not a vessel tracker.
- Multimodal journeys (truck, air, last mile) arrive with #80; the model
  already has a mode per leg.
- The ontology layers and the knowledge-graph deep dive (on #30, later).
