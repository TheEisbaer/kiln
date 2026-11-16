---
status: accepted
---

# Claude Code is the only agent harness

Kiln runs its agents on Claude Code and on nothing else: no OpenCode, Copilot, Codex or other harness, now or in the public version. Everything else about a setup (operating system, Git host, kind of target repo) may vary later; the harness does not. This lets Kiln use Claude Code's own features directly instead of the common subset of several harnesses.

## Considered options

- **Self-hosted Claude Managed Agents** (looked at on 2026-11-14, two days after its private beta opened): Anthropic's Managed Agents server run on one's own infrastructure, with a sandbox per session, closed network by default and credentials kept outside the sandbox. Rejected as Kiln's base because it is a different harness, not a way to host Claude Code:
  - its own agent loop runs in the server, and it does not discover a repo's `.claude/skills`, so a target repo's Claude Code configuration would not apply (ADR 0025);
  - the advisor setting is refused on a self-hosted server (ADR 0030);
  - it needs PostgreSQL as a second store (ADR 0032) and, on Kubernetes, gVisor nodes and a sandbox controller;
  - the download comes only through an Anthropic account team under its own licence, which a colleague or a public user cannot be asked to obtain (ADR 0003);
  - its price is not published.

  It does confirm the shape of ADR 0017. It becomes an option again only if this decision itself is given up; Kiln would then be a board on top of that server.

## Consequences

- No harness abstraction layer is built or kept open.
- Kiln depends on Claude Code's behaviour and release cadence, and has to handle its changes.
