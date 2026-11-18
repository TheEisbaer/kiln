---
status: accepted
---

# Review blocks only on the spec, a concrete failure or a written standard

Review enforces standards and catches bugs, but a finding sends a card back only if it can point to one of three things: the spec (the change does less or more than was approved), a concrete failure (a bug, a security or data-loss risk, missing error handling, a concurrency or performance problem, a regression), or a written standard of the target repo. Anything else is discarded. Each round has a fresh reviewer, and if every finding blocks, each finds new small things the last did not, so the loop does not converge.

1. Review produces a verdict and findings, nothing more. Remarks are not written anywhere: with automatic merging nobody reads them, and for human reviewers they are noise.
2. Review looks only at what the card changed.
3. The implementing agent gets the same written standards before it writes code.
4. Standards a tool can check (formatting, lint, types) are checks, not review.
5. Standards change only by the developer's decision. No agent raises or lowers the bar.
6. The loop stops after two send-backs. A third review with findings brings the card to the developer, counted as an unplanned intervention.

## Considered options

- **Judging only against the spec**: rejected; with automatic merging and no human reading the code, the codebase would degrade.
- **Every finding blocks**: rejected; it is what keeps the loop from converging.
- **Recording discarded remarks to spot missing standards**: rejected for now as machinery ahead of need. When merged code is not what the developer wants, they write the standard down.

## Problems outside the card's change

A real problem the reviewer notices that the card did not cause, such as an existing bug next to the changed lines, does not block the card. Left open here at first and settled by ADR 0033: it is proposed to the developer, who can turn it into a card or dismiss it.
