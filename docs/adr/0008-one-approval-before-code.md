---
status: accepted
---

# One planned intervention per card: approval before any code is written

Every card stops once by design: the developer approves what will be built before implementation starts. After that Kiln runs to ready to merge without them, and any further intervention is unplanned and counts against the attention goal (ADR 0007). The approval sits there because agreeing to a short statement of what will be built is cheap, while discovering a disagreement in a finished pull request is not: by then the work has to be redone or is thrown away.

## Considered options

- **No planned intervention**: rejected; it moves the disagreement to the most expensive place.
- **A second approval on the pull request**: rejected; code review is the reviewing agent's job plus whatever the target repo demands.
- **Stopping only for cards judged big or unclear**: not now. It needs Kiln to judge card size, which is a source of complexity of its own. To be revisited with recorded data on which kinds of card never needed the approval.

## Consequences

- Kiln records, per card, whether the approval changed anything, so the "it depends on the card" option can be judged on data later.
