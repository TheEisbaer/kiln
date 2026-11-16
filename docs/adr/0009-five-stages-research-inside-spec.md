---
status: accepted
---

# Five stages; research is a step inside spec, not a stage

A card passes through backlog, spec, implement, review and done. Research (what exists, what is out there, how it could be built) happens inside the spec stage rather than as a stage of its own, and "ready to merge" is the state in which review ends rather than a stage. A stage boundary is kept only where a result is handed to someone who decides; with one approval per card (ADR 0008) nothing is decided between research and spec. With research as a stage of its own, the research report becomes what the developer actually approves, the spec stage after it has nothing left to decide, and the board gains a lane.

| Stage | Result | Then waits for |
|---|---|---|
| Backlog | a written card | the developer, to start it |
| Spec | what will be built | the developer, for approval |
| Implement | the change | nobody; goes to review |
| Review | pass, or back to implement | the Git host, for the merge |
| Done | merged | nobody |

## Consequences

- The spec carries the research: what was found, which options were considered, which was chosen and why, so the developer can overrule the choice at approval.
- How the spec is produced (one agent or several, one pass or more) is not visible as stages.

## Revisit when

- The developer wants to choose between options before a spec is written, on most cards.
- Cards that end in a report instead of a change come into scope.
- Recorded data shows approvals often overturning the chosen approach.
