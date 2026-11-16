---
status: accepted
---

# A spec is one short document of Kiln's own, borrowed once from Spec Kit

A spec is a single document of about a page with the same sections every time, so that it can be approved in about a minute (ADR 0008) and review can judge the change against it (ADR 0015). Its template starts from GitHub Spec Kit, as a one-time borrow rather than a fork: Spec Kit's work is not re-derived, but Kiln takes no code from it and does not follow its releases.

Spec Kit as it is does not fit what a spec has to be here. It is a sequence of commands with a person deciding between them, where Kiln has one approval per card; it produces several documents in a numbered folder and branch per feature, where a card is one pull request; and it writes its files and a constitution into the repo, which in a team repo is the team's call. Used as it is, one feature comes to several files and hundreds of lines, and its numbering collides when each run numbers from its own branch.

Taken from Spec Kit:

- requirements written so that they can be checked;
- explicit markers for what is still unclear;
- written standards, in place of its constitution (ADR 0015).

The sections, roughly: what will be built; how to tell it works; what was found, the options considered and the one chosen; what is deliberately left out; questions for the developer. The exact sections are settled in design.

## Considered options

- **Spec Kit as it is**: rejected for the reasons above.
- **A real fork that tracks Spec Kit**: rejected; most of it (its command-line tool, scripts and support for other agents) would be deleted, and the merge work would continue for as long as it is tracked.
- **A separately named kit that also works without Kiln**: not now; a second thing with its own users. Its own decision later if it is wanted.

## Consequences

- Spec Kit is MIT-licensed (copyright GitHub, Inc.). Where wording is copied from its templates, its copyright and permission notice is kept with it.
- The spec template is part of Kiln and has no name of its own.
