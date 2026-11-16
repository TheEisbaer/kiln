---
status: accepted
---

# Kiln is its own first target repo; a closed phase list triggers the move to the work repo

Kiln is developed with Kiln first, to drive it forward on a repo the developer fully controls, and the developer's work repo follows once the basic flow works. A tool developed on itself tends to stay there, since "once it works" never clearly arrives; a phase list with rules replaces it.

## The rules

1. Two phases. POC: the flow works on Kiln's own repo. MVP: it works on the work repo, for the developer and at least one colleague.
2. Items are outcomes that can be checked by watching, not features or components. Each phase has a short list, around five to seven items.
3. The list is closed at the end of the design session. Adding an item later means removing one, or recording the reason as a decision.
4. The work repo is the first MVP item. When the POC list is checked off, the next work is support for the work repo, and nothing new is built for Kiln itself before it.

## Considered options

- **The work repo first**: proves Kiln on the hard case from the start; rejected by the developer in favour of getting the basic flow working on a repo without a team and company pipelines around it.
- **A card count or a date as the trigger**: rejected as arbitrary; the phase list ties the switch to what Kiln can do.
- **Deciding when it feels ready**: rejected; without a written condition the move keeps being put off.

## Consequences

- The first Git host Kiln supports is whichever one hosts Kiln's own repo. ADR 0026 settles that as GitHub. The work repo is on Azure DevOps today and is moving to the same GitHub, so a second host is needed only if Kiln is wanted on the work repo before that move.
