---
status: accepted
---

# A moving repo sends a card back to implement; a spec that cannot be built sends it back to spec

The target repo keeps changing while a card is in progress. Kiln reacts only when something is actually wrong, and uses the stages it already has instead of separate machinery. Keeping every branch current all the time is machinery of its own, and it still ends in conflicts that need the developer.

1. **Merely behind: nothing.** A pull request that can still merge is left alone.
2. **Cannot merge: back to implement.** When the Git host reports a conflict or a failed required check, the card returns to implement exactly as if review had sent it back, and review runs again. No intervention.
3. **The spec cannot be built as written: back to spec.** Implement or review can return a card to spec, whatever the cause (the repo moved, or the spec was wrong). The agent raises it with a reason; the developer decides at approval. This is an unplanned intervention and is counted.
4. **No coordination between cards in progress.** Kiln does not predict or prevent overlaps between branches.

The line between 2 and 3: would the result differ from what was approved? If only the code has to be adjusted, it is a conflict; if what gets built would change, it is a spec problem.

## Consequences

- A revised spec is written knowing what is already on the card's branch, and says what is kept and what is redone.
- An agent that returns cards to spec too readily shows up in the intervention count; one that quietly builds something else is what review against the spec has to catch.

## Settled later

- A card that returns to implement from the Git host counts as a send-back, for a conflict as for a failed check (ADR 0054).
- A card that returns to spec has its spec session resumed, since that stage gets its own work back; after the new approval implement starts in a fresh session and the number of send-backs starts again (ADR 0053, ADR 0054). That approval is not the planned one and is the intervention counted here (ADR 0042). How a run reports that the spec cannot be built is listed in docs/open-points.md.
