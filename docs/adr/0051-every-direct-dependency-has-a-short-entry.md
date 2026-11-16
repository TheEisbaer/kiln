---
status: accepted
---

# Every direct dependency has a short entry in one file, and only choices with lock-in get an ADR

Dependencies change by a card, deliberately (ADR 0023), and a spec already states what was found, the options considered and the one chosen (ADR 0021). Two things were missing. With automatic merging nobody reads the code, so an agent can add a library that no spec named; and the reasoning in a spec lives in the store, where a later run does not see it, so a second library for the same job can arrive.

Every direct dependency therefore has one entry in one file in the repo. An entry has four parts: what the dependency is for, what else was looked at, why this one, and its licence. The licence is there because Kiln's repo is public (ADR 0049).

1. **The spec names any new dependency,** so the developer's one approval covers it (ADR 0008). A dependency the approved spec did not name is a finding, since the change then does more than was approved (ADR 0015).
2. **The check command fails when a direct dependency has no entry** (ADR 0045). A rule a tool can decide is a check, not review.
3. **A replaced choice is marked as replaced,** not deleted, so the reason for leaving it stays readable.
4. **Choices with lock-in still get an ADR** when their card comes, for example the embedded database (ADR 0032, ADR 0037), the way UI components are built (ADR 0023) and the description of the API (ADR 0044).

## Considered options

- **Nothing beyond the spec and the occasional ADR**: the smallest, but the reason for a small choice is invisible to later runs, and nothing stops a dependency no spec named.
- **One full record per choice**, with context, decision, rationale and consequences, as teams with customers and licence obligations keep them: every choice has its why, at the price of a document per library for a tool with one developer, and such records tend not to be kept up.

## Consequences

- One more rule lives in the check command.
- The first cards are built by hand, before Kiln can run a spec stage; their entries are written by hand as well.
