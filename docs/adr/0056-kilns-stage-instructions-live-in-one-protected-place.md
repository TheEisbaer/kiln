---
status: accepted
---

# Kiln's stage instructions live in one protected place

What Kiln tells a run is part of Kiln's code: the spec template (ADR 0021), the review rules (ADR 0015) and how a run works (ADR 0025). A run uses the instructions of the installed Kiln, not those on a card's branch, so a card cannot weaken the review that judges it. It could weaken review for every card after it, and merge automatically while doing so. ADR 0006 therefore counts review instructions among the guardrails, but the named places of ADR 0047 did not include them.

All stage instructions live in one top-level directory, `instructions/`, and that directory is a protected place. A new instruction file is then protected by where it is put (ADR 0047), and the code owner file gains one line.

## Considered options

- **Only the review rules and the spec template protected, implement instructions free**: fewer interventions, but two places and a blurry line, since implement instructions also carry rules such as running the check and not pushing.
- **Not protected, left to review**: against ADR 0006.

## Consequences

- Every change to an instruction waits for the developer (ADR 0007). Early on that will be often, and it is accepted because these files are how Kiln's behaviour is steered. If the recorded interventions show that they dominate, the first option is taken up again with numbers.
