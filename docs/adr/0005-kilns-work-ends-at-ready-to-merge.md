---
status: accepted
---

# Kiln's work ends at "ready to merge"; merging belongs to the target repo's Git host

Kiln takes a card as far as a pull request that is ready to merge. Whether and how it merges is governed by the target repo's Git host and that repo's rules, because repos differ: the developer's work repo requires green CI, two reviewers and a linked work item, which Kiln cannot and should not control. A target repo can opt in to automatic merging, off by default, and then the host performs the merge with its own auto-merge and required checks. Merge machinery of Kiln's own (a merge train, a merge check, keeping branches current, merging) would be among its largest parts and would repeat what the host already does.

## Scope for the POC

Kiln's own repo opts in to automatic merging so that cards the developer writes can land without them. Two further steps toward self-improvement were considered here:

- **Automatic update**: the running instance replacing itself with the newly merged version. First deferred, then moved into the POC by ADR 0019.
- **Its own ideas**: Kiln creating cards for itself and working on them. Left out of the POC and decided later, with what the POC has shown.

## Consequences

- Kiln builds nothing for merge ordering, keeping branches current or resolving merge conflicts at merge time.
- A card is done when the host reports its pull request merged; Kiln observes that, it does not cause it, except by having switched on the host's automatic merge.
