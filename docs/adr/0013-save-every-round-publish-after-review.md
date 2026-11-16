---
status: accepted
---

# Work is saved after every implement round and published only when review has passed

Saving and publishing are separate acts. After every implement round, including failed ones, a card's work is saved to that card's branch in the instance's own copy of the target repo, so nothing of value exists only in a run's working directory and working directories can be deleted at any time. The work is published (pushed to the Git host as a pull request) only when review has passed, and again after any later implement round that passes review. Work that exists only in a working directory is stranded when its run ends badly, and then has to be rescued by hand.

Publishing is held back because a push or a pull request sets things off on the host: CI runs, colleagues see the work, reviewers are notified. What exactly it sets off is the target repo's business, as it is for a human; Kiln's part is to push rarely and to use a branch naming pattern configured per target repo.

## Considered options

- **Push after every round**: protects against losing the machine, but costs a CI run per round and puts unfinished work in front of reviewers.
- **Keep work in the working directory until publish**: rejected; this is how work gets stranded.

## Consequences

- Unpublished work exists only where the instance keeps its copy. Losing that loses agent hours, not the developer's own work.
- Wherever agents run, the instance's own copy must be durable and reachable from them. This is a requirement on the runtime decision.
- The host's checks run after the agent's review; a failure there costs another implement and review round.
