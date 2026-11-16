---
status: accepted
---

# A card is one self-contained pull request; order is a hold on starting

A card is one idea, written by the developer, small enough to become exactly one pull request in exactly one target repo. It builds only on what is merged in that repo at the moment the card starts, not when it was written, because a spec researched against an older state describes code that may no longer exist. A card can name predecessors and is held in the backlog until they are done. Ideas too big for one card are split into separate cards that remember their origin. Without a size rule a card grows over many runs and revisions and is split only after the work has started.

## Considered options

- **A card as one idea of any size, with several pull requests**: rejected; "done" becomes ambiguous and the spec too long to approve quickly.
- **Parent cards with child cards**: rejected; a parent needs its own state, approval meaning and rules for dropped children, which turns Kiln into a project-management tool. The origin reference gives the grouping without the state.
- **Building a card on another card's unmerged branch**: rejected; when the earlier card changes in review the later one has to be redone, and Kiln would have to manage branches that depend on branches (see ADR 0005).

## Consequences

- Cards from a split are separate but not necessarily independent; predecessors express their order.
- A card with predecessors has its spec written only after they are done, so it is researched against a repo that contains them.
- A change that spans two repos is two cards.
