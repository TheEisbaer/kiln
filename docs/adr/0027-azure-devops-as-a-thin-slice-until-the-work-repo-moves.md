---
status: accepted
---

# Azure DevOps is supported as a thin slice for the work repo, built to be deleted

The developer's work repo is on Azure DevOps and moves to GitHub next year; the developer wants Kiln working on it before that. Azure DevOps therefore becomes a second Git host at the MVP, when a named person needs it (ADR 0003), but only as far as the work repo needs. Merging in the work repo is governed by the host and the team (ADR 0005), so Kiln sets up no rules, runs no checks and merges nothing there. Three things remain:

1. **Publish:** push the card's branch and open a pull request.
2. **Link a work item:** The work repo's pull requests cannot merge without one. The developer gives the card the work item's number, and Kiln links it when it opens the pull request. Kiln does not create, search or synchronise work items.
3. **Observe:** learn that the pull request was merged or abandoned, or cannot merge.

## Considered options

- **Full parity with GitHub**: rejected; it builds a great deal for a host with a known end date.
- **Git only, with the developer opening the pull request and reporting the merge**: rejected; two manual steps on every card for the work repo, and Kiln blind to what happens after publishing.

## Consequences

- The slice is removed when the work repo has moved, unless someone else is using it by then. The old path is not kept beside the new one.
- Automatic merging, guardrail setup and checks exist only for GitHub.
