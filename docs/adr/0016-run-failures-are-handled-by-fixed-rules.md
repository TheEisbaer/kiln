---
status: accepted
---

# Run failures are handled by a few fixed rules, not by a supervising agent

When a run fails for a reason that is not about the code, Kiln deals with it by three rules, and no agent supervises the instance. Most such failures are environmental (idle timeouts, vanished processes, credential, network and memory failures). Handling each kind with a mechanism of its own grows without end, into many retry paths and many kinds of warning, and an agent that supervises the remainder can cost more than the runs it supervises.

1. **One automatic retry.** A failed run is started again once, from the last saved state.
2. **Then the card stops and needs the developer**, with the error, as a single kind of "needs you". This is an unplanned intervention and is counted.
3. **A failure that is not about one card pauses the instance.** If Claude cannot be reached or credentials have expired, Kiln stops starting runs and tells the developer once, instead of failing every card in turn.

If the same failure keeps reaching the developer, its cause is fixed or a rule is added, as a decision.

## Later

A supervising agent is wanted in a later phase, not the POC. It joins automatic update and Kiln's own ideas (ADR 0005) as steps to be decided each on its own, with what the POC has shown.
