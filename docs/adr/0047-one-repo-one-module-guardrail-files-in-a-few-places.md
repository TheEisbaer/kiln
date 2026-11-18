---
status: accepted
---

# One repo with one Go module, divided by Kiln's parts, with guardrail files in a few named places

Kiln's repo is one repository holding one Go module at its root, laid out by Go's own convention for a repo with several programs and private packages, and divided by the parts of ADR 0034: the core, the proxy and the launcher. The UI has its own top-level directory and is built into the core's program (ADR 0044). This follows Go's official guidance, which shows a single module throughout and gives no rule for non-Go parts, and the layout of a comparable tool with a server, an agent and a web UI in one repo.

What is Kiln's own is where guardrail files go. A file that defines a guardrail (ADR 0006) lives in one of a few named places: the check command and its configuration (ADR 0045), the image definitions (ADR 0039), the launcher (ADR 0036), the workflow and code-owner files, and the repo's Claude Code configuration and written standards (ADR 0025). The protected path list names those places and not single files, so a new guardrail file is protected by where it is put and cannot be forgotten on a list.

1. **Each part keeps to itself.** What two parts must agree on, the description of the API and what the core pushes to the proxy, sits in one shared place.
2. **Exact directory names are chosen by the first card.**

## Considered options

- **Go's convention only, with scripts, configuration and image files wherever convenient**: guardrail files scatter, the protected path list grows file by file, and a forgotten entry is an unprotected guardrail.
- **Several repos for the core, the UI and the images**: a card is one pull request in one repo (ADR 0010), so most changes would need two cards, and the description of the API would live across repos.
- **Several Go modules in one repo**: separate dependencies per part, with no present need and more for agents to get wrong.

## Settled later

- One more named place: Kiln's own stage instructions, in `instructions/` (ADR 0056).
- The places of the seal, which ADR 0048 names as protected, are `cmd/proxy/` and `internal/seal/`; the image of the proxy is under `images/`.
- Only `internal/seal` runs the `docker` command. A step of the check command fails when a Go file outside it that is not a test contains the string literal `"docker"`; it stops a slip, not an evasion.
- The core is `cmd/core`, built as `kiln`, and the steps of a run are `internal/run`, not `internal/core`: a package named after the whole core would take everything. Both call `internal/seal` and do not run the `docker` command.
- The step of the check command also fails on a string literal whose first word is `docker`, as in a shell line, or that is a path ending in `/docker`.
