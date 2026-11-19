---
status: superseded by ADR 0049
---

# The checks runner is isolated like a hosted runner, not sealed like a run

> Superseded by ADR 0049: Kiln's repo is public and its checks run on the Git host's own runners, so there is no runner on the developer's machine. The reasoning on run sealing in the last consequence below still stands.

ADR 0017 seals runs, and ADR 0026 puts Kiln's checks on a runner on the developer's machine, which executes agent-written code before anyone has approved it. No decision said how that runner is protected. It is an isolated container: nothing from the developer's home directory, no access to the container engine, memory and CPU limits, and a workflow token that can only read. Its network stays open, and the core does not manage it.

A run and the runner differ. In a run an agent acts: it chooses commands and reads outside content, with permission prompts off. On the runner only code executes that the agent wrote and review has already passed, which is an ordinary job of continuous integration. What others do, read on 2026-11-16, follows the same line: GitHub's hosted runners are a clean, isolated virtual machine per job with an open network, and its advice for self-hosted runners is isolation, clean environments and as little sensitive information as possible, without restricting the network; agent sessions, by contrast, are sealed by their vendors, with limited or blocked network by default and credentials kept outside.

## Considered options

- **Sealed like a run and managed by the core**: one seal for both places. Rejected as more than anyone does for such jobs; the core would gain a container to look after, and a clean start for every job needs either administration rights on the repo or home-grown automation, which GitHub's guidance warns against.
- **Leaving Actions for a checker of Kiln's own**: exactly a run, but it reopens ADR 0026, where it remains the stated fallback.

## Consequences

- Checked code can reach the network, including services on the developer's machine, and a job could leave something behind for the next. Both are accepted while the only code checked there is Kiln's own.
- POC item 5 keeps testing the seal of runs only.
- A public Kiln would not keep this runner: self-hosted runners are advised against for public repos, and public repos get hosted runners.
- Run sealing was questioned in the same discussion, since the code Kiln works on is the developer's and their colleagues'. ADR 0017 stays as decided. Its reasons are the agent's own mistakes made with everything the developer can reach, untrusted content inside trusted repos, guardrails staying out of the agent's reach, and runs not coming to rely on one machine; none of them is hostile colleagues.
