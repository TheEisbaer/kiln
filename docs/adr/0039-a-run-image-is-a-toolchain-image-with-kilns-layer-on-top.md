---
status: accepted
---

# A run image is a target repo's toolchain image with Kiln's layer on top

Three decisions left open who defines a run's image: Claude Code is pinned in it and changed by a card (ADR 0020), Kiln's settings for a target repo live in the instance and nothing of Kiln's is added to that repo (ADR 0025), and every target repo needs its own toolchain. A run image is therefore built from two parts. Kiln's repo defines Kiln's layer: Claude Code at its pinned version and whatever a run needs from Kiln. The instance names a toolchain image per target repo. Kiln builds the run image as that toolchain image with its own layer on top.

There is then one Claude Code version per version of Kiln, changed by a card and tested with the core that reads its output, whatever the number of target repos; and the toolchain stays the business of the instance.

1. **For Kiln's own repo the toolchain image is defined in Kiln's repo.** It is that repo's own build environment, used by its checks and by the build of ADR 0037 as well. Because it decides what the checks run in, its definition is a protected path (ADR 0006).
2. **For any other target repo the developer supplies the toolchain image** and names it in the instance. Kiln adds its layer and never builds another repo's toolchain.

## Considered options

- **One image per target repo, defined wholly in the instance**: one file per repo says everything, but the Claude Code version would sit where no card can change it, once per repo, while the core depends on one version's output format.
- **Kiln's part copied into the container at every start**: any toolchain image works unchanged and nothing is built, but Claude Code alone is a 365 MB program copied per run, and what a run contained is harder to state afterwards.
- **One image with every toolchain**: grows with every repo, and a change for one repo rebuilds all.

## Consequences

- Kiln's layer installs onto images Kiln did not make, so a toolchain image has to meet a short list of requirements.
- An update that touches Kiln's layer rebuilds one run image per target repo.
