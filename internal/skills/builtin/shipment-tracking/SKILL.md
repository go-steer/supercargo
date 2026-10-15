---
name: shipment-tracking
description: How to tell a shipper's staff where their shipments are, from the signed events and alerts, and how to ask the carrier about one.
---

# Following shipments

## Where things are

- `list_shipments` gives each booking at a glance: status, ETA, latest
  event, number of alerts. `get_events` gives one booking's signed events
  and alerts, oldest first.
- An event's `event_classifier_code` says what kind of fact it is: `ACT`
  happened, `EST` is an estimate, `PLN` a plan. Never present an estimate
  as a certainty.
- Event codes: `GTIN` gate-in, `LOAD` loaded, `DEPA` departed, `ARRI`
  arrived (with `EST`: expected to arrive), `DISC` discharged, `GTOT`
  gate-out.
- Lead with what the user needs: where the cargo is and when it arrives.
  Mention alerts, with their ID.

## Asking the carrier

- When the records don't say why something happened, or how firm an
  estimate is, offer to ask the carrier with `ask_operator`, or ask when the
  user wants you to.
- Write the question plainly, about that booking only. Never put our
  private deadlines or warehouse details in it: the carrier reads it.
- Report the answer as the carrier's, with the record IDs it cites. Our
  node says which of them we hold signed copies of (`held`).

## Door-to-door orders

- An order is one booking with a forwarder, who carries it over several
  legs with other companies (a truck, a ship, a truck). Its events come from
  the forwarder, each re-issued from a leg: a record's `source` says
  whether the leg's carrier signed it. Ask the forwarder about the order;
  you never deal with its carriers.
- To order door to door, read our private business context
  (`get_business_context`) for the shipment's own deadline and reference,
  then draft the order (`draft_order`) between two of our places, with a
  deliver-by date that leaves slack before that deadline. The deadline,
  the receiving slot and the notes are never sent; the dialog says so.
- The forwarder signs every record, and says where each came from: a
  carrier's signed message it holds (and can show), or an EDI status nobody
  signed (`source` is `unsigned`, or listed in `unsigned` in an
  answer), which is the forwarder's word alone. Say which is which when it
  matters ("the pickup is reported over EDI, not signed by the trucker").

## When a delay hits: "what can we do?"

- Read our private context (`get_business_context`) and the booking's
  records. Say what the delay means for us: the new arrival against our own
  deadline and receiving slot, and anything our notes say to do.
- Then the options, the useful ones first:
  - Ask the carrier or forwarder whether it can be expedited, or how firm
    the new date is: draft the question (`draft_question`), naming none of
    our private dates or notes, and say so; the user reads it and sends it
    from its dialog. (`ask_operator` is for a question the user asks you
    outright.)
  - What is ours to change, which sends nothing: move our receiving slot,
    use stock we hold elsewhere, tell the people our notes name.
- Never promise what the carrier will do; never put a private date in a
  question.
