---
status: accepted
---

# Kiln may change workflow files, and the developer says yes before such a change is published

ADR 0049 kept Kiln from changing workflow files altogether: the folder has a code owner, and on top of that Kiln's app was never given the permission the Git host asks for, so the Git host itself refused the push. Workflow files changed only through the developer's own pull request.

That has a price. By 2026-11-20 the check on the Git host had grown from 40 seconds to nine minutes, and half of what was left after the tests ran side by side was building from nothing on every run. A cache is a change to a workflow file, so it was work by hand, as every change to the checks on the Git host would be. On another repo Kiln could take no card about its checks at all.

A code owner alone does not replace the missing permission, and the reason is when things happen. A pull request runs its own version of a workflow file (ADR 0049, rule 3), and it runs it when the branch is pushed. The code owner's approval decides the merge, which comes later. A workflow file written by a run would therefore be carried out on the Git host's runner before the developer has seen it, with the token permissions the file itself asks for.

1. **Kiln's app has the permission to change workflow files.** The developer gives it in the app's settings.
2. **A change that touches a workflow file is published only after the developer has seen it and said yes.** The yes comes before the push, because the push is what makes the Git host run the file.
3. **The yes is asked where the change is published.** While the developer publishes by hand (ADR 0052), the publishing step shows the change to the workflow files and asks. When the core publishes, the screen marks such a change and asks there.
4. **A session never gives that yes.** A publishing step started by a session refuses a change to a workflow file, as before.
5. **The folder keeps its code owner.** The merge still waits for the developer's approval on the Git host.

## What this changes in earlier decisions

- **ADR 0049, rule 3:** "Kiln's app does not have it and is never given it" and "Workflow files therefore change only through the developer's own pull request, and it is the Git host that keeps Kiln from changing them." Kiln's app has the permission, and what keeps a workflow file by a run from running unseen is the developer's yes before the push. The rule's first two sentences hold: a pull request runs its own version of the workflow file, and the folder has a code owner.

## Considered options

- **Leave it as it was:** nothing a run writes runs on the Git host unseen, and the Git host enforces it. Every change to the checks stays work by hand.
- **Give the permission and rely on the code owner:** the least to build. A workflow file by a run is then carried out before the developer has looked at it.
- **Let the developer's own account open such pull requests for Kiln:** the app keeps its narrow permission. A pull request by the code owner is not held (ADR 0049, rule 5), so this would remove the approval on the Git host for exactly these changes.

## Consequences

- The Git host no longer stops a workflow file by Kiln. A fault in the publishing step that skips the question lets such a file run. The step's test for it is therefore part of the card that lets the core publish.
- What a workflow file can reach is worth keeping small: Kiln's repo has no secrets, its workflow asks for read access to the contents only, and nobody can push to the main branch.
- The developer reads a workflow change twice, before the push and at the approval on the Git host. The first is the one that counts for what runs.

## Revisit when

- The repo gets a secret that a workflow can read.
- Kiln works on a repo where a workflow can deploy something.
