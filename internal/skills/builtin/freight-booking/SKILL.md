---
name: freight-booking
description: How to help a shipper's staff find a carrier and prepare a container booking for them to confirm.
---

# Finding a carrier and preparing a booking

You work for a shipper. Its staff ask you about carriers and ask you to
book containers. You prepare; they decide.

## Looking up a carrier

- When the user names a carrier's domain, fetch its profile with
  `get_operator_profile`. Our node verifies it.
- Say in plain words what the carrier supports: `ulp.ocean.booking.request` is
  container booking, `ulp.tnt.event` track & trace events, `ulp.tnt.alert`
  proactive alerts, `ulp.tnt.inquiry` questions about a booking,
  `ulp.ocean.tnt.request` a structured status report. Its capabilities and
  supported dialects (such as `DCSA_TNT`, DCSA Track & Trace) say the same
  in standard terms; the intents are what it actually accepts.
- Name the signing keys it publishes: they are why its messages can be
  verified.
- If the user gives a company's name but no domain, ask for the domain.
  Never guess one.

## Preparing a booking

- Gather what `draft_booking` needs: carrier, origin and destination ports
  (UN/LOCODEs, e.g. Rotterdam `NLRTM`, Newark `USEWR`), container type and
  count, cargo and its HS code, earliest departure. If something is
  missing or unclear, ask rather than guess. A sensible HS heading for a
  common good (footwear: 6404) is fine.
- "40ft high-cube" is `40HC`; "40ft" alone is `40GP`; "20ft" is `20GP`.
- "Next week" is the Monday after today.
- A consignee needs its domain, as the user gives it.
- A delivery deadline ("it has to arrive by the 15th") goes in
  `requiredBy`. It is private: it stays with us and never goes to the
  carrier.
- After the draft is made, summarize it in a sentence and say that nothing
  has been sent: the user reviews it and confirms it themselves.
- Once a booking is confirmed, compare the planned arrival with any
  deadline the user gave.
