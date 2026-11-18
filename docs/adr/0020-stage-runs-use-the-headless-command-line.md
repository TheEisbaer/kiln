---
status: accepted
---

# Stage runs use Claude Code's headless command line, and nothing calls a model outside a run

Kiln invokes Claude Code in one way: the command line in headless mode, inside the sealed container (ADR 0017). The Agent SDK is not used. It starts and supervises that same command-line program as a child process, so a run started through it dies with the process that started it, while Kiln restarts itself after its own merges and runs must survive that (ADR 0019). Inside a sealed container the SDK would also need a program of Kiln's own in every run image, and its main extra, deciding tool permissions in code, matters less when the container is the boundary.

1. Anything that uses tools or touches a repo is a stage run through the command line, and goes no other way.
2. There are no model calls outside a stage in the POC. Everything a model does in Kiln happens inside a run.
3. If a short call outside a run is needed later, it is decided then, on its own. It would not need sealing, since a call without tools cannot act on anything.
4. The Claude Code version is pinned in the run's image and changed by a card, never "latest".

There are three possible paths: the command line for stage runs, the SDK for short calls (model routing, a command bar, rewording and splitting cards) and the API directly for speed. Kiln uses only the first. Such short calls are either the spec stage's work here or conveniences the POC does not need.

## Consequences

- Kiln depends on the format of the headless event stream, which carries no stated stability guarantee; pinning the version is what contains that.
- Kiln's own implementation language is not constrained by the SDK's languages.
- Anthropic's documentation says third-party products may not offer claude.ai login without approval. The developer's gateway setup is unaffected, but it limits how a public Kiln can be used and has to be settled before going public.
