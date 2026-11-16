---
status: accepted
---

# Human review feedback comes to the developer first

When a person reviewing a published pull request comments or asks for changes, the card comes to the developer with the comments, and the developer chooses: send it to implement with those comments, answer it themselves, or send it back to spec. Kiln does not act on human feedback by itself. The reviewers are reviewing the developer's work and talking to the developer; not every comment is a request, and an agent that treats a question as an instruction changes code nobody asked to change.

1. **Kiln never writes in a conversation on the Git host.** It writes a pull request's title and description when publishing, and pushes commits. Replies to people are the developer's.
2. **It counts as an intervention.** The common case is one action ("address these"), and the count shows how often human review sends work back.

## Considered options

- **The developer handles it entirely, with Kiln only showing that feedback exists**: rejected; the common case can be one action.
- **Comments go straight to implement as if review had found them**: not now. To be revisited for kinds of comment the recorded data shows the developer always passes on unchanged.

## Consequences

- A human reviewer's comment is also the best signal for a missing written standard (ADR 0015), since it is something Kiln's own review did not catch.
- A published card can come back for three reasons: it cannot merge (ADR 0012), a person asked for changes, or the pull request was abandoned.
