---
status: accepted
---

# Kiln scans everything it is about to publish and refuses on a hit

Kiln's repo is public (ADR 0049), so a secret or a name that must not appear there cannot be taken back once it is pushed. Publishing is the only thing that writes to a Git host (ADR 0017), so that is where the scan sits. Before Kiln publishes, it scans what is about to leave the developer's machine: the change, the commit messages, the branch name, and the pull request's title and text. A hit means nothing is pushed and the card needs the developer (ADR 0016).

1. **Two kinds of hit.** Secrets, found by a scanner's rules for keys and tokens, and forbidden names, from a list the developer keeps.
2. **The list of forbidden names lives in the instance** (ADR 0025), never in a repo. In a public repo the list would itself be the leak, and in the instance no run can reach it.
3. **The Git host's own secret scanning stays on** as a second net. It works after the push and knows only common token formats, which is why it is not the first.
4. **Until Kiln publishes by itself,** the same scan runs as a hook in the developer's clone before every push.

The scanner is gitleaks: it is the common open-source tool for this, under the MIT licence, declared feature complete by its maintainers, and it takes rules of one's own beside its defaults.

## Considered options

- **Scanning in the repo's checks**: simple and visible, but the checks run after the push, when the content is already public, and a public repo cannot hold the list of names.
- **Relying on the Git host's scanning alone**: nothing to build, but it acts after the fact and knows no names.

## Consequences

- A false hit costs the developer an intervention (ADR 0007).
- The list is the developer's to keep current; Kiln cannot know which names matter.
- This applies to every target repo Kiln publishes to, not only its own; a private repo simply has less at stake.
