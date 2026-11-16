---
status: accepted
---

# The instance has state and history of its own, on the same stream

Two facts belong to no card: that the instance is paused (ADR 0016), and that automatic merging is off after a failed update (ADR 0019). The store held cards and runs (ADR 0032), and the event stream carried the history of cards only (ADR 0044), so nothing said where these facts live or how the UI hears of them.

The instance is therefore one more record in the store, with state and history written together in one step, exactly as for a card (ADR 0042). The one event stream carries the entries of cards and of the instance. The UI then hears of a pause at once, "says so once" (ADR 0016) survives a restart of the core, and pauses can be counted like everything else.

A proposal needs none of this: it belongs to the card whose review made it (ADR 0033), so it is an entry of that card.

## Considered options

- **Not stored, worked out on each tick, with the UI asking for it**: nothing new is stored, but the UI would have to ask again and again, which ADR 0044 decided against, and that automatic merging is off has to survive a restart in any case.
- **A special card that stands for the instance**: no new kind of record, but every rule about cards would need an exception for it.

## Consequences

- An entry on the stream has one of two kinds of subject, a card or the instance, and the UI handles both.
- Any further fact about the instance is added to this record by a decision, in the same way as a fact about a card (ADR 0054).
