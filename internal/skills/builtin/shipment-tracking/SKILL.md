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
  the forwarder, each re-issued from a leg: `derived_from` says from what.
  Ask the forwarder about the order; you never deal with its carriers.
- The forwarder signs every record, and says where each came from: a
  carrier's signed message it holds (and can show), or an EDI status nobody
  signed (`derived_from` with `signed: false`, or listed in `unsigned` in an
  answer), which is the forwarder's word alone. Say which is which when it
  matters ("the pickup is reported over EDI, not signed by the trucker").
