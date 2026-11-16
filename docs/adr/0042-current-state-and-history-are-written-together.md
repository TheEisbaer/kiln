---
status: accepted
---

# A card's current state and its history are written together

Kiln is judged by measures it counts itself (ADR 0007): interventions per merged card, how long a card waited for the developer (ADR 0022), how often each ceiling is hit (ADR 0029), whether an approval changed anything (ADR 0008), proposals made and accepted (ADR 0033). All of them are questions about what happened. Every change to a card is therefore written as the card's new state plus one history entry, in a single atomic step (ADR 0032). The entry says what changed, when, and who caused it: the developer, a run, the Git host or a rule. Counters written apart from the state they count drift, and facts such as when a card merged go missing.

1. **Measures are queries over the history.** They cannot disagree with the state, and a new measure works for the past as long as the change it counts was recorded.
2. **The cause is on every entry.** An intervention is a change caused by the developer outside the planned approval; nothing counts interventions separately.
3. **The history is never read to decide what to do.** The core acts on current state only (ADR 0035). The history is for measures and for showing the developer what happened.

## Considered options

- **Current state only, with counters and timestamps on the card**: the smallest, but each new measure needs a new field, cannot be computed for the past, and can disagree with the state it counts.
- **The history as the truth, with current state computed by replaying it**: complete, but every read goes through replayed state and every kind of entry must stay readable forever.

## Consequences

- Every change to a card goes through one place in the code, since that is the only thing that keeps the two writes together.
