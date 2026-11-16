---
status: accepted
---

# Two limits on cards in progress, enforced by queueing

Kiln limits how many cards are in progress with two numbers, and a card is held in the queue by whichever is reached first. They protect different things and therefore count different things. A limit that is only a written rule is enforced by cancelling cards by hand, after more are in flight than the developer can follow.

| Limit | Protects | Counts |
|---|---|---|
| Per target repo | the repo and its reviewers: overlapping changes, open pull requests | every card in progress, including ready to merge |
| Per instance | the developer and their machine: approvals and problems arriving at once, agents and checks running side by side | cards in progress that are not yet ready to merge |

Starting a card beyond a limit queues it; when a slot frees, the first queued card that fits both limits starts. The developer can change the numbers, and Kiln never exceeds them. Starting values: 3 per target repo, 3 per instance.

## Considered options

- **No limit**: rejected; more cards are then in flight than the developer can follow.
- **One at a time**: a special case (limit 1); rejected as the default because one card waiting for approval or for reviewers blocks everything, and approvals cannot be batched.
- **A limit per stage**: not now; it needs several numbers tuned before there is data on which stage is the bottleneck. It can be added later.
- **One definition of "in progress" for both limits**: rejected; cards waiting days for colleagues in one repo would then block every other repo while needing nothing from the developer or the machine.

## Consequences

- The per-repo limit is built for the POC. The instance limit is built with the second target repo, because with one repo it can never be the limit that blocks.
- A card waiting for reviewers holds a slot in its repo. That is deliberate; the number can be set per repo.
