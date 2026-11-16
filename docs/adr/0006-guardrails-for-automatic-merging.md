---
status: accepted
---

# Guardrails for automatic merging

A target repo that opts in to automatic merging (ADR 0005) merges a pull request without the developer only when all of the following hold. Rules kept in prose, or in code and settings that the agents being judged can change, do not hold, and a rule stated in two places ends up disagreeing with itself.

1. **Required checks pass.**
2. **A review by an agent with a fresh context** finds nothing blocking.
3. **No protected path is touched.** Protected paths always need the developer. They are configured per target repo, not hardcoded, and that configuration is itself protected.
4. **One switch** turns all automatic merging off.

**Principle:** a guardrail only counts if the agent whose work is being judged cannot change it in the same pull request.

## Where checks run

Left open here at first, under two constraints: no recurring cost for CI, and the principle above. Settled by ADR 0026: checks run in the Git host's CI on a runner on the developer's own machine, and the host enforces their result.

## Consequences

- Anything that defines a guardrail (check configuration, review instructions, permission and credential settings, the protected path configuration) belongs on the protected path list of a repo that merges automatically.
