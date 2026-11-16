---
status: accepted
---

# Until Kiln builds its own cards, its flow is run by hand

Kiln is developed with Kiln (ADR 0004), but the first cards have to be built before Kiln can run a stage. In that stretch nothing enforces the rules that keep the work in bounds: one approval before code (ADR 0008), review that blocks for three reasons only (ADR 0015), a limit on cards in progress (ADR 0014) and a measure (ADR 0007). The cards of that stretch are therefore built with ordinary Claude Code sessions that follow Kiln's own flow, under five rules.

1. **Spec first.** One session writes the spec, one page in the sections of ADR 0021, and the developer approves it before any code is written.
2. **Fresh sessions.** One session implements, and a new one reviews the change against the spec, sending it back only for the reasons of ADR 0015.
3. **One card in progress at a time,** until Kiln enforces its limits itself.
4. **Published as Kiln's app, by the developer.** The developer runs the publishing step with the app's key; a session is not given the key or a token. A pull request by the developer is not held on a protected path (ADR 0049), so only the app's pull requests meet every guardrail.
5. **A step that Kiln can do is never done by hand again.** The developer keeps a tally of interventions per card, which is the first value of the measure.

This is how tools that build themselves usually start: the first stage is built by other means, in the shape of the real process, and each step is handed over as soon as the tool can do it.

## Considered options

- **Spec Kit as it is**: ready to use, but rejected for Kiln's flow by ADR 0021, it would commit its files to a public repo, and it would rehearse a different flow from the one being built.
- **Ordinary sessions without rules**: the fastest start, but nothing holds scope, approval or review, and nothing is learned about the spec format or the review rule before they are built.

## Consequences

- Slower per card than working without rules, and it holds only by the developer's discipline until Kiln enforces each rule.
- These sessions are not sealed, so keeping the key out of them rests on the developer's discipline, like the other rules of this stretch.
- The exact sections of the spec (ADR 0021) are settled by using them on these cards.
- Specs and the tally of this stretch are kept outside the repo, as the store will keep them later (ADR 0032).
- The stretch ends with the first POC outcome, a card that goes from idea to merged with one approval.
