---
status: accepted
---

# One supported setup first; more setups only when someone needs them

The vision is a public Kiln that supports other operating systems, Git hosts and kinds of target repo. The first version supports exactly one setup: the developer's own, shared by a few close colleagues with the same hardware, Claude setup and work repo. Each thing expected to vary is handled in one known place with one implementation, and a second implementation is added only when a named person is about to use it. Implementations built ahead of need tend to go unused, and each one still has to be kept working.

## Considered options

- **Extension points now, one implementation each**: rejected, because seams designed before a second real case exists tend to be the wrong ones.
- **Two implementations now to prove the seams**: rejected as cost without a user.

## Consequences

- No plugin mechanism, installer or settings for setups nobody on the first version has.
- "Public later" still rules things out today: nothing that works only by accident of one machine, and no secrets or company names in the code.
