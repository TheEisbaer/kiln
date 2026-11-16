---
status: accepted
---

# Kiln optimises the developer's attention first, quality second

Kiln is judged first by how little of the developer's attention a merged card costs, measured as interventions per merged card, and second by quality, measured as the share of merged cards that need a follow-up fix. Throughput (merged cards per week) and cost per merged card are tracked but are not goals. Without a stated measure, momentum becomes one: card and commit counts look like success while the developer's day goes into supervising.

## Consequences

- Kiln counts interventions and follow-up fixes itself; a measure that needs manual bookkeeping does not count.
- Where a design choice trades attention against throughput or cost, attention wins; where it trades attention against quality, attention wins only if quality does not get worse.
- Cost is recorded per card for information. A ceiling that stops a runaway run is a separate matter from cost as a goal.
