---
status: accepted
---

# Kiln's settings for a repo live in the instance; the repo's own Claude Code configuration is picked up from the repo

How Kiln treats a target repo (protected paths, automatic merging, the limit, branch naming, which checks run) is configured in the instance, outside every repo. Nothing of Kiln's is added to a target repo, which in a team repo would be the team's call, and no run can reach these settings, which is the strongest form of "the agent cannot change its own guardrail" (ADR 0006). Each setting exists in one place, with no overrides. Configuration spread over several places with overrides ends with nobody sure which one counts.

The repo's own standards and Claude Code configuration stay in the repo and apply to Kiln's runs: instruction files, skills, agents, hooks, plugins and MCP servers. Its owners put them there deliberately, so a run should behave as a session started in that repo would. Four limits:

1. **Only what is in the repo.** Nothing from the developer's home directory: runs are sealed (ADR 0017), a colleague's instance must behave the same on the same repo, and personal instructions are written for a person at a terminal.
2. **Read from what is merged, not from the card's own changes.** Otherwise an implementing agent writes the rules its reviewer works under. These files belong on the protected path list of a repo that merges automatically.
3. **Network and credentials only where the instance allows them.** MCP servers that reach outside services, and plugins that are normally downloaded at start, work only if the proxy allows the host or the plugin is already in the run's image.
4. **The repo decides how code is written; Kiln decides how a run works.** Repo instructions about committing, pushing or asking a person mid-run do not apply.

## Considered options

- **Kiln's settings committed in the target repo**: travels with the code and is shared between colleagues, but puts one developer's tool into a team repo and within reach of the agents it governs.
- **Both, with one overriding the other**: rejected; it is how configuration drifts into several places.
- **Ignoring the repo's Claude Code configuration**: rejected; a run would then not behave as a session started in that repo would.

## Consequences

- Colleagues each configure a shared repo in their own instance. Sharing a configuration between instances is added when a second person needs it.

## Settled later

- Protected paths and the required checks are not settings in the instance. For a repo that merges automatically the Git host enforces both, from the code owner file in the repo and from its own rules (ADR 0047, ADR 0049), and the checks are a command in the repo (ADR 0045). Each still has one place. The instance holds what a host cannot: whether automatic merging is used, the limit, branch naming and the toolchain image (ADR 0039).
