---
status: accepted
---

# A card ends as done or dropped, and only the developer drops it

A card ends in one of two ways: done (its pull request is merged) or dropped (it will not produce a change). The spec stage can therefore have two results, "this is what will be built" or "nothing should be built, and why", and either reaches the developer at the same point, where they approve the spec or confirm the drop. Kiln proposes a drop but never drops a card on its own, because a card that disappears without the developer seeing why costs more trust than the confirmation costs attention. An agent rarely says "do not build this" unless that is an ordinary result of its stage, and cards that should not be built are then abandoned by other routes, often after work has been done.

## Consequences

- Dropped is an ending, not a stage; the developer can drop a card from any stage.
- A proposal to drop a card names the cards that have it as a predecessor, so the developer decides about them at the same moment instead of leaving them unable to start.
- Dropped cards do not enter interventions per merged card; the share of cards dropped is tracked as its own number.
- Dropped is separate from hiding old cards from view. An archive that serves both mixes cards that were decided against with cards that are merely old.
