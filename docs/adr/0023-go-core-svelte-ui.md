---
status: accepted
---

# Kiln's core is written in Go, its UI in Svelte

Kiln's core (holding cards, starting and watching runs, talking to a Git host, serving the UI, updating itself) is written in Go, and the UI is a Svelte client application.

Go fits decisions already made: a single binary makes automatic update a matter of build, swap and restart, and undoing it a matter of keeping the old binary (ADR 0019); fast compiles and cached test results keep the checks short (ADR 0031); and it is the native language of containers and Kubernetes, which come at the MVP (ADR 0018). The developer does not write Go daily and chose it knowingly, being willing to learn it. Svelte stays because the developer knows and likes it, has agent tooling for it, and agents work well with it.

## Considered options

- **TypeScript throughout with SvelteKit**: one language and the simplest POC; rejected for what a single binary gives the update and its undo (ADR 0019).
- **C# core**: the developer's and their colleagues' daily language, with Microsoft's own libraries for Azure DevOps and Kubernetes; rejected for slower builds and tests and heavier images than Go.
- **Rust, Python**: slow fix loops for agents, and the least automatic checking, respectively.
- **React or Vue for the UI**: React is what the developer's work repo uses and has the largest component ecosystem; neither solves a problem Kiln's UI has.

## Consequences

- Two languages: two toolchains in every run image for Kiln's own repo, and an explicit API between core and UI instead of shared types.
- No SvelteKit server side; the core serves the UI.
- One way to build UI components is chosen once, in the design session, and not swapped mid-project.
- Dependencies change by a card, deliberately, not continuously in the background.
