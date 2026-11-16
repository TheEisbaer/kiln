---
status: superseded in part by ADR 0049
---

# Kiln's repo lives in the developer's personal space on their company's GitHub; checks run on the developer's machine

Kiln's own repo is a private repo under the developer's own account on their employer's GitHub Enterprise. That makes GitHub the first Git host Kiln supports (ADR 0004), and it settles where required checks run and who enforces them (left open in ADR 0006): they run in GitHub Actions on a runner on the developer's own machine, and the host refuses the merge through a ruleset. Kiln builds no checker of its own.

The choice was made on what the platforms offer at no recurring cost. A personal repo there has host-enforced rulesets and Actions, and a runner registration token was granted for one on 2026-11-13. GitHub's hosted runners are not available to repos owned by managed accounts, so checks use the developer's machine, which the cost constraint asked for anyway. The developer's work repo is moving to the same GitHub, so one host family covers Kiln now, the work repo after its move, and a public Kiln later.

## Considered options

- **A personal Azure DevOps organisation**: its free tier appears to offer every guardrail, and it is the work repo's current host. Rejected once it was clear that repo is leaving Azure DevOps, and because Azure DevOps is retiring public projects.
- **A public GitHub repo from day one**: everything free, including hosted runners; rejected because the code and its history would be public from the first commit.
- **A private GitHub repo on a paid personal plan**: a monthly fee, with a charge for self-hosted runner minutes announced once and only postponed.
- **GitLab's free tier**: cannot require the developer's approval by path, and is a host family nobody around the project uses.
- **An organisation on the company's GitHub**: hosted runners would be billed to the employer, which does not fit a personal project (ADR 0024).

## Consequences

- Kiln acts on the host under its own identity, not the developer's, so that approving a change to a protected path means approving something the developer did not author.
- Going public means pushing the repo to a personal account on public GitHub; a managed account can only own private repos visible to the enterprise.
- Some marketplace actions do not work on this kind of host; Kiln's own workflows avoid depending on them.
- The work repo moves next year and Kiln is wanted on it before then, so Azure DevOps is supported for a limited time as a thin slice (ADR 0027).

## Cost

Claude usage for Kiln is allowed and signed off by the developer's employer. CI cost is not, so Kiln's repo must generate no billable Actions usage:

- checks run only on the developer's own runner; hosted runners cannot be used by a personal repo on this host in any case;
- workflows upload no artifacts and keep caches on the runner, so nothing is stored on the host;
- no hosted background jobs such as automated dependency updates; dependencies change by a card (ADR 0023).

Self-hosted runner minutes are free today. GitHub announced a per-minute charge for them on private repos in December 2025 and postponed it without a new date. If it is introduced, checks move off Actions altogether: a checker outside the agents' reach runs them and reports the result to the host as a commit status, which a ruleset can require just the same.

## Confirmed on a scratch repo, 2026-11-13

On an empty personal repo on this host, with a runner in a container on the developer's machine:

- the runner registered and a job ran on it within seconds;
- a ruleset requiring that job's check and a pull request refused both a merge and a direct push while the check was queued;
- with automatic merge switched on, the pull request merged by itself 9 seconds after the runner came back and the check passed.

Everything created for the test was removed afterwards.

## Still to confirm in the POC

- Requiring the developer's approval by path (code owner review in a ruleset).
- Kiln acting under its own identity, as an app installed on the repo.

If either is not possible for a personal repo, the fallback is a public GitHub repo.

## Settled later

- How the checks are defined (ADR 0045).
- How the runner is protected: isolated like a hosted runner, not sealed like a run (ADR 0046).
- Where tests run that need the container engine, which the runner does not have (ADR 0048).
- Kiln cannot act as an app on a personal repo on this host, so the fallback named above was taken: a public repo on github.com under the developer's personal account, with checks on the host's own runners (ADR 0049). GitHub as the first Git host, Kiln building no checker, and Kiln acting under its own identity all stand.
