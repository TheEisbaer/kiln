---
status: accepted
---

# Kiln updates itself after a merge to its own repo

Automatic update is part of the POC: when a card in Kiln's own repo merges, the running instance replaces itself with the merged version. ADR 0005 first deferred this; it was moved forward because updating by hand after every merged card is an intervention per card (ADR 0007), because the developer wants to see merged work in use at once, and because it is cheap while Kiln runs from a checkout (ADR 0018). The simplest way, fast-forwarding the running checkout and restarting, kills runs in flight.

Three conditions:

1. **Only for Kiln's own repo.** The instance updates when one of its own cards merges.
2. **Runs survive the restart** and are picked up again afterwards; a sealed run does not depend on the Kiln process staying up.
3. **A failed update undoes itself.** If the new version does not start properly, Kiln returns to the previous one, turns automatic merging off, and needs the developer.

## Consequences

- With automatic merging, a card can go from approval to being part of the running instance without the developer; the guardrails of ADR 0006 are what stands in between.
- On Kubernetes the same behaviour means an image build and rollout per merge; that is designed when Kubernetes arrives.
- Kiln creating its own cards and a supervising agent remain deferred.
