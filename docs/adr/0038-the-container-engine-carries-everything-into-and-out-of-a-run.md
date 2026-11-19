---
status: superseded in part by ADR 0059
---

# The container engine carries everything into and out of a run

> Revised in part by ADR 0059: "a run has no network but the proxy" no longer holds for a run that has a list of hosts. It also reaches the hosts on its list.

Kiln and its runs share no filesystem (ADR 0018), and a run has no network but the proxy (ADR 0017). What crosses the boundary of a run is carried by the container engine: the core copies the code and the instructions into the container before it starts, follows its output through the engine, and collects the results from the container after it has stopped. Nothing is mounted, no script inside the run talks to Kiln, and no run holds a secret of its own.

Stated without naming an engine, a run receives an image, files placed before the start and an environment, and yields an output stream, an exit status and files collected after the end.

1. **The change** is collected from the working tree and saved by the core (ADR 0013). The agent's own commits, if any, do not count.
2. **A stage's documents** (a spec, a verdict, findings, proposals) are files the agent writes to one fixed directory outside the working tree, collected the same way. ADR 0017's "results out only through save" therefore reads: results leave a run only through what the core collects after it has ended.
3. **The output stream is for watching.** It gives progress and the time of the last output (ADR 0029). The complete record of a run is collected once at the end.
4. **A stopped container keeps its files and its output** until the core has collected them (ADR 0035), so work can be recovered even from a run that was killed hard.

Measured on 2026-11-14: after a hard kill the container's files could still be copied out and its output read until the container was removed; reading the output with timestamps inserted a timestamp every 16 KB inside a line, which breaks large events.

## Considered options

- **Git over the network, through the proxy**, with a script in the run that fetches the code and pushes the result: the same on Docker and on Kubernetes. Rejected because the proxy would gain a second job and need the target repo copies, each run would need its own secret to push only to its own place, a run killed hard would have pushed nothing, and all of it would be built before any Kubernetes run exists.
- **A directory of the developer's machine mounted into the run**: ruled out by ADR 0018, and it leaves stale working directories behind.

## Consequences

- Kubernetes cannot copy files out of a pod that has ended. Placing and collecting files is the one place that is rebuilt at the MVP, where the whole path of a card is tested on k3s in any case. "Ready for Kubernetes without rework" is given up here in exchange for building nothing for it in the POC.
