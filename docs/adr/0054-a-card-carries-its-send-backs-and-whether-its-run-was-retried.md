---
status: accepted
---

# A card carries its number of send-backs and whether its run was retried

Three rules need to know something about a card's past: the loop stops after two send-backs (ADR 0015), a failed run is retried once (ADR 0016), and implement moves to a stronger model after a send-back (ADR 0030). A card's state was two closed lists (ADR 0043), and the history is never read to decide what to do (ADR 0042), so nothing said where these facts live.

A card therefore carries two more small facts beside its stage and what it waits for: the number of send-backs since its spec was approved, and whether the run it waits for has already been retried. They are state like the two lists, written in the same step as the history entry (ADR 0042), so they cannot differ from it, and the core keeps acting on current state only (ADR 0035).

1. **The stronger model is not stored.** It follows from a number of send-backs above zero.
2. **The retry belongs to one run.** The flag is set when a failed run is started again and cleared when a run of that stage finishes.
3. **A conflict or a failed check on the Git host counts as a send-back.** The card returns to implement exactly as if review had sent it back (ADR 0012), so the loop stays bounded whoever sends it back: when the number is already two, the card needs the developer instead of a third round (ADR 0015).

Others keep such facts with the unit of work in the same way: a job queue stores the retry count in the job, and Kubernetes keeps the failed count in a Job's status and compares it with its limit. Systems that derive it from the history are those in which the history is the source of truth, which ADR 0042 decided against.

## Considered options

- **Derived from the card's run records in the store**: no new fields, but the rules become queries with an implied "since when", and run records are history in all but name.
- **Derived from the history**: against rule 3 of ADR 0042.

## Consequences

- A card's state is no longer only two lists. Any further fact a rule needs is added the same way, by a decision, and not as a new stage or a new kind of waiting.
- When the developer acts on a card under "needs you", that action says what the two facts become. Which actions exist is still open.
