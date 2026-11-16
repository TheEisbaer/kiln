---
status: accepted
---

# The POC runs on the developer's machine; Kubernetes is the second runtime

In the POC, Kiln runs from a checkout on the developer's machine and sealed runs (ADR 0017) are started by the local container engine. Kubernetes (k3s) is the intended second runtime and arrives at the MVP, when a colleague needs to install Kiln. The developer wants Kubernetes for one deployment recipe on any host, for resource limits and for isolation; containers already give the last two, and the first is not needed until an instance has to run somewhere else.

A move into a cluster fails when runs have come to rely on the developer's machine (home directory, git identity, local paths). To keep Kubernetes from being an afterthought:

- runs are sealed from the first day, so a run is a pod in all but name;
- before Kubernetes is relied on, the whole path of a card is tested on it automatically.

## Considered options

- **k3s from day one as the only runtime**: avoids any later move and matches the developer's existing setup. Rejected for the POC because cluster work appears on no POC outcome and can consume a project, and because updating Kiln after each merge costs an image build and rollout there.
- **Runs directly on the host with Claude Code's sandbox**: rejected; see ADR 0017.

## Consequences

- Nothing in Kiln may depend on Kiln and its runs sharing a filesystem.
- What Kubernetes adds beyond containers (chart, image distribution, network policy, secret delivery) is built at the MVP, each part only as it is needed.

## Settled later

- A run is not quite "a pod in all but name": placing files in a run and collecting them from it uses the container engine and is rebuilt for Kubernetes at the MVP (ADR 0038).
- Kiln no longer runs from a checkout. Each version is a program built in a sealed container, with the previous one kept beside it (ADR 0036, ADR 0037).
