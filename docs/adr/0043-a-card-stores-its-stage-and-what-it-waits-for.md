---
status: accepted
---

# A card stores its stage and what it waits for; a run is in flight or has ended in one of four ways

The state of a card is two short closed lists, the two columns ADR 0009's table already has: the stage, and what the card waits for. The terms in CONTEXT.md name combinations of the two and are not stored as states of their own. Without closed lists states multiply: every new situation becomes another lane or another kind of "needs attention".

| Stage | The card may wait for |
|---|---|
| Backlog | the developer, to start it · room (queued) |
| Spec | a run · the developer's approval · the developer (needs you) |
| Implement | a run · the developer (needs you) |
| Review | a run · the developer (needs you) · the Git host (ready to merge) |
| Done | nothing |

1. **Dropped is a mark on the card,** not a value in either list; the card keeps the stage it was dropped from (ADR 0011).
2. **"Needs you" has no kinds.** It carries its reason as text and points to what caused it (ADR 0016).
3. **"Waits for a run" is the core's instruction to itself.** On each tick a card that waits for a run and has none in flight gets one started (ADR 0035).
4. **Only valid pairings can be written,** enforced in the one place through which every change goes (ADR 0042).

A run has no waiting state of its own; a card waits for a run. A run is in flight, or it has ended:

| Ending | Meaning | What follows |
|---|---|---|
| Finished | it produced its stage's result | the card moves on |
| Failed | it ended for a reason that is not about the code | one retry (ADR 0016) |
| Hit a ceiling | Kiln ended it at one of the three ceilings | no retry; the card needs the developer (ADR 0029) |
| Withdrawn | Kiln ended it because its card no longer needs it | nothing |

## Considered options

- **One field with a value for every combination**: one thing to look at, but values multiply with every addition, and "everything that waits for the developer" becomes a list to keep up to date.
- **Only the stage stored, the rest worked out each time**: nothing can drift, but "needs you" and an approval are decisions, not facts that can be read off elsewhere, so the core would have to decide from the history, against ADR 0042.

## Settled later

- "The developer's approval" covers confirming a proposed drop, since a spec run ends with one of two results (ADR 0011).
- A card carries two more facts beside the two lists: its number of send-backs and whether its run was retried (ADR 0054).
- `change` in `internal/store` is the one place of rule 4, and the 11 valid pairings are one table in its code (ADR 0057).
