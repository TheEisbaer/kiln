---
status: accepted
---

# Each stage starts with a fresh session, and a stage that is sent back resumes its own

A stage's run starts a new Claude Code session. It sees the repo and what Kiln hands it, the card and the spec, and nothing of an earlier run's conversation. A clean context gives better work for less, and it is what makes review independent (ADR 0006, ADR 0015).

The exception is a stage that gets its own work back. When the developer does not approve a spec and says why, or review sends a change back to implement, the next run of that stage resumes the earlier session. The agent then still knows why it built what it built and deals with the findings only.

1. **Review never resumes.** Every round has a fresh reviewer (ADR 0015).
2. **Starting fresh must always work.** If the earlier session cannot be resumed, because it is lost, a newer Claude Code cannot read it after an update (ADR 0019), or it belonged to a run that failed (ADR 0016), the run starts fresh from the saved work (ADR 0013), the spec and the findings. Resuming is the better path, not a second requirement.
3. **The record of a session is a result of its run.** It is carried out of the run and into the next one like any other result (ADR 0038), and kept until the card is done or dropped.
4. **There is no third correction of an implementation.** After two send-backs the card stops (ADR 0015), so a resumed implement session never holds more than two failed attempts.
5. **A spec has no such bound in Kiln.** The developer takes part in every rejection and can drop the card or rewrite it when the spec keeps missing. Rejections per card can be counted from the history (ADR 0008); if cards with three or more show up, starting the next spec run fresh is added then, by a decision.

This follows Claude Code's own guidance, read on 2026-11-16: build from a spec in a fresh session; have the change reviewed by a session that did not see it being written, and give the findings back to the session that wrote it; after two failed corrections start over instead of trying a third.

## Considered options

- **Every run fresh, also after a send-back**: nothing to carry and no holding on to a wrong approach, but each round pays for reading everything again and the agent has lost its reasons.
- **One session for a whole card**: the cheapest hand-over between stages, but the reviewer is no longer independent and the context grows with every stage.

## Consequences

- A headless run can resume by a session's identifier or by the path to its record, by Claude Code's documentation. Resuming in a new container from a record that was carried there is not yet tested.
- A send-back moves implement to a stronger model (ADR 0030), so a resumed session continues on another model than it began with. Not yet tested.
- A resumed run reports the cost of the whole conversation, earlier runs included, by the same documentation. The budget ceiling (ADR 0029) has to allow for that.
- In the stretch built by hand the same rule applies, and rule 2 of ADR 0052 is reworded to say so.
