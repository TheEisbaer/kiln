---
status: accepted
---

# Kiln's checks are one command that runs the tools directly in the toolchain image

The checks on Kiln's own repo (ADR 0026, ADR 0031) are defined as one command in the repo that runs the tools directly: tests, lint, type checks. It is run inside the toolchain image (ADR 0039) in every place: by the implementing agent in its sealed run before review, by the Git host's workflow on the runner, and by the developer by hand. The image is what makes the result the same everywhere. An agent that can run the checks it will be judged by finds a failure before publishing, where it costs nothing, and not after, where it costs an implement and a review round (ADR 0013).

The rules of ADR 0031 (sizes, per-test limits, CPU time, the list of unreliable tests) live in this command.

## Considered options

- **Dagger**, which the developer knows from their work repo and which has checks as a built-in concept with ready modules. Rejected for Kiln's own repo, on what its documentation said on 2026-11-16:
  - its command line needs an engine, and the engine is a privileged container, which a sealed run does not have, so the agent could not run the checks;
  - giving a run an engine breaks the seal: whoever can connect to the engine can run containers on it, those containers are not behind Kiln's proxy, and no setting was found that restricts their network;
  - on the runner, the pull request's own version of the check code would drive that engine before anyone has approved anything;
  - it adds a privileged, long-lived service with its own disk to the developer's machine, and nothing merges while it is down;
  - it is in the middle of a breaking move to its version 1.0;
  - nothing was found that supplies the rules of ADR 0031, so they would be written either way;
  - its strength is orchestrating containers, which Kiln's checks do not need.

  In the work repo the first three do not arise, because the engine's clients there are trusted pipelines, and it has the orchestration that Dagger is for.
- **Steps written directly in the workflow file**: nothing extra, but they cannot be run inside a sealed run at all.

## Consequences

- Running checks in parallel, selecting them and reporting are added to the command when they are needed.
- MVP item 5, a sealed run that can build and test the work repo, meets the same question: its checks need an engine, credentials and an open network. It is decided at the MVP whether the agent runs the work repo's underlying tools directly or its runs get an engine that is itself behind the proxy. If the second is built, Dagger for Kiln's own checks is reconsidered then.

## Settled later

- "The same everywhere" holds for what the check command can run inside the toolchain image. Tests that need the container engine run in the checks on the host only (ADR 0049); how they are started there is settled by the first card that has such a test.
