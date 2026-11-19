---
status: accepted
---

# Kiln's repo is public on GitHub, under the developer's personal account

ADR 0026 put Kiln's repo as a private repo under the developer's account on their employer's GitHub, with Kiln acting on the Git host as an app of its own, and named a public repo as the fallback if that proved impossible. It did. Confirmed on 2026-11-16: an account managed by an enterprise can create an app but cannot install it on its own account. The install page offered only an organisation, the address for the developer's own account answered "not found", and the Git host's documentation says that standard apps cannot be installed on such accounts. Without a second identity the developer would be both the author and the required approver of the same pull request, which the Git host refuses, so "a protected path always needs the developer" (ADR 0006) could not be enforced.

Kiln's repo therefore lives as a public repo on github.com under the developer's personal account. There, on the free plan, repository rules and code owners are available, an app can be installed on a personal account, and the developer set as a condition that the Git host must cost nothing.

## What changes

1. **Checks run on the Git host's own runners,** which are free for public repos and give every job a fresh virtual machine. There is no runner on the developer's machine, so ADR 0046 no longer applies.
2. **Tests that need the container engine run in the checks,** because each of those machines has one. Only tests that need the developer's Claude credential are still run by hand. This revises ADR 0048. The code that defines the seal stays a protected place (ADR 0006).
3. **The rules of ADR 0031 stand.** Hosted machines vary in speed as well, so nothing fails on elapsed time there either.
4. **The toolchain image (ADR 0039) has to be obtainable by those runners.**
5. **Everything is public from the first commit:** code, history, every pull request and the output of every check. "Nothing company-specific goes into the repo" (ADR 0024) becomes a rule whose breach cannot be undone, and commits carry an address of the personal account, not the employer's.
6. **A public repo is not a public product.** ADR 0003 stands: one supported setup. Kiln acts only on its own cards and on pull requests it opened itself, whatever others file on the repo. "Going public" on the later list of the phases now means supporting other people's setups.

## Considered options

- **A private repo on a paid personal plan**, about four dollars a month: the only option that changes no other decision and can still be made public later. Rejected by the developer, for whom the Git host must be free.
- **Staying on the employer's GitHub with a workflow opening the pull request**, so that the Git host's own automation is the author and the developer can approve: free and private, but with four untested parts, one more guardrail in a workflow file, and possibly forbidden by the enterprise.
- **A private repo on the free plan of github.com**: repository rules and code owners are limited to public repos there, so the Git host would enforce nothing.
- **GitLab's and Bitbucket's free plans**: neither can require an approval or enforce a merge check without a paid plan.
- **A personal Azure DevOps organisation**: its free tier covers every need; ruled out by the developer.
- **A Git host run by the developer**: its features could not be confirmed, and colleagues could not reach it.

## Consequences

- Publishing cannot be taken back; making the repo private later does not remove copies.
- An agent's slip, such as an internal name in a test, is public at once. Sealed runs hold no credentials (ADR 0017), which keeps the damage small.
- Anyone can file issues and pull requests on the repo and comment on Kiln's. That matters once review comments are shown to the developer (ADR 0028).

## Confirmed on a scratch repo on 2026-11-16

A public repo on the free plan, with one rule set on the main branch: pull requests only, one required check, review by the code owner required with no further approvals, and nobody allowed to bypass.

1. **Kiln can act under its own identity.** An app with read and write access to contents and pull requests, and nothing else, was installed on the personal account for one repo only. Its pull requests carried the app as author, the checks ran for them, and commits made through the Git host's API were signed by the Git host.
2. **Approval by path holds for the app.** Its pull request on a free path merged without the developer. Its pull request on a path with a code owner was refused with "waiting on code owner review", the app could not approve its own pull request, and after the developer's approval it merged by itself.
3. **A pull request runs its own version of the workflow file.** The folder with the workflow files therefore has a code owner, like every guardrail file (ADR 0047). An app also needs a separate permission to change workflow files at all. Kiln's app does not have it and is never given it: on 2026-11-16 the Git host refused the app's push of a workflow file. Workflow files therefore change only through the developer's own pull request, and it is the Git host that keeps Kiln from changing them.
4. **Automatic merging can only be switched on while something is still outstanding.** On a pull request whose checks had already passed the Git host refused it, and the app merged directly instead. Publishing needs both paths.
5. **A pull request by the code owner is not held.** With no further approvals required, the developer's own pull request on a protected path was mergeable at once. That fits: the rule exists to hold Kiln, not the developer.
