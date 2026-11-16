---
status: accepted
---

# A problem review notices outside a card's change is proposed to the developer, not acted on

When a reviewing agent notices a real problem that the card did not cause, such as an existing bug next to the changed lines, it does not block the card (ADR 0015) and Kiln does not create a card for it. It is kept as a proposal and shown to the developer on the first screen, where one action turns it into a backlog card and another dismisses it. Discarding a genuine bug without trace is a loss, and the note costs almost nothing because the review's output is stored anyway; but creating cards from what agents notice is how a backlog outgrows what the developer can read.

1. **Only concrete problems are proposed:** a bug, a security or data-loss risk. Matters of taste are still discarded.
2. **A proposal is not a card.** It holds no slot, is in no stage, and becomes a card only when the developer accepts it. An accepted proposal remembers the card it was noticed on.
3. **Proposals are shown apart from what needs the developer.** They block nothing, so they do not count as "needs you" and do not raise its count.
4. **Kiln records how many proposals were made, accepted and dismissed.** That is the evidence for deciding later whether some kinds should become cards automatically.

## Considered options

- **Discarding them**: loses real problems.
- **Keeping them only as a note on the card**: nobody opens a merged card to look.
- **Creating backlog cards directly**: deferred with "Kiln's own ideas" (ADR 0005) until the recorded numbers support it.

## Consequences

- A card is written by the developer or accepted by them from a proposal; Kiln still never creates one on its own.
