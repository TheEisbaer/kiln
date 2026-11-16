---
status: accepted
---

# Every run has three ceilings, and a run that hits one is not retried

A run that has gone wrong is stopped by one of three ceilings: elapsed time, time without any output, and budget. This is separate from cost as a goal, which Kiln does not have (ADR 0007). Ceilings set far above the normal case stop nothing: a run that has gone wrong then uses many times what a normal run takes before anything ends it.

1. The ceilings are set per stage, in the instance (ADR 0025), and are the same for every card. No card and no agent can raise them.
2. A run that hits a ceiling is not retried. The one automatic retry of ADR 0016 is for failures such as a lost connection; a run that ran out of time or budget would most likely do so again. The card needs the developer.
3. Raising a ceiling is the developer's decision. Kiln shows how often each ceiling is hit, so one that is too tight is visible, and so is one that drifts upward.

## Consequences

- Claude Code's headless mode limits turns and budget itself but has no limit on elapsed time, so the time ceilings are enforced by Kiln from outside the run.
- Starting values are set in the card that builds the ceilings, as a small multiple of the normal case, and corrected from Kiln's own run data.

## Confirmed on 2026-11-16

With the Claude Code version of that day, in a sealed container behind the proxy:

- The budget ceiling ends a run after the model call that crossed it, never before the first call, so it can be overshot by one call.
- The run reports this as an ending of its own, distinct from any other, and what it wrote before is still in its working directory.
- The cost it counts is Claude Code's own estimate at list prices, not what the developer's gateway bills.

How the core recognises a run that hit a ceiling, and that the time ceilings hold across a restart, is in ADR 0035 and ADR 0043.
