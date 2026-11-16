---
status: accepted
---

# The UI shows what needs the developer first; the board is second

In the POC the UI is a web page served locally with three screens and nothing else. The first screen is what is waiting for the developer, not the board, because the goal is the developer's attention per merged card (ADR 0007) and a board shows where everything is rather than what needs them.

1. **Needs you:** specs to approve, drops to confirm, cards that stopped, and whether the instance is paused.
2. **The board:** cards in the five stages with their state; here the developer writes a card, orders the backlog, starts a card or drops one.
3. **A card:** its spec, what its runs did, and review's findings.

Left out of the POC on purpose: a settings page (configuration lives in a file), a command bar, drag and drop, themes, and any second way of doing something the three screens already do. A UI easily becomes the largest single part of such a tool, and each extra surface is code that is rebuilt or deleted later.

## Kiln does not interrupt

In the POC Kiln sends no notifications. The "needs you" screen is where the developer looks, with the count visible in the browser tab. Being called over for every event works against the attention goal; looking when one chooses and handling several things at once works for it. The price is waiting, since a card that needs the developer holds a slot (ADR 0014), so Kiln records how long each card waited. If that shows cards sitting for hours, one channel is added then.

## Considered options

- **A full board first**: rejected for the POC; it is the picture of the flow, not the place where the developer's attention is spent.
- **No UI of its own (a command-line tool, or cards as issues on the Git host)**: rejected; issues would tie what a card is to one host's features while the host is still undecided.

## Consequences

- The UI may become richer after the POC.
- The visual design is Kiln's own, worked out in its own design session, not in the POC.

## Settled later

- The first screen shows everything that waits for the developer: specs to approve, drops to confirm, and cards in the state "needs you". The term itself keeps its narrower meaning from CONTEXT.md, a card that has stopped outside the planned approval.
