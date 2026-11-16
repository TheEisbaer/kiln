---
status: accepted
---

# Kiln is a core on the host, a proxy in a container, and one detached container per run

Kiln has three kinds of process. The core is a single process on the developer's machine, started from a checkout (ADR 0018), and it is the only one an automatic update restarts (ADR 0019). The proxy (ADR 0017) is a separate long-lived container on the runs' network, which an update of the core does not touch. Each run is a detached container that the container engine keeps alive; it is not a child of the core.

The proxy is not part of the core because a sealed run could not reach it there. Measured on the developer's container engine on 2026-11-14: a container on a sealed network reaches no process on the host, only other containers on the same network, while a proxy container attached to both the sealed network and an ordinary one is reachable by name and passes on nothing it does not forward itself. Keeping the proxy out of the core also means that a restart, a crash or a failed update of the core does not cut the network of runs in flight.

## Considered options

- **One process, with the proxy inside the core**: the smallest, with one thing to start, log and update. Rejected because it is unreachable from a sealed network unless the core itself runs in a container, which ADR 0018 rejected for the POC. Outage tolerance was not the reason: headless Claude Code retried refused connections ten times over two to three minutes and rode out outages of 5, 30 and 120 seconds.
- **The UI server split from the core as well**: the UI would stay up during an update; rejected because it puts two writers on one store (ADR 0032) and no POC outcome needs it.

## Consequences

- Two things have to be started and kept alive, the core and the proxy.
- The Claude credential has to get from the developer's machine into a container; how is its own decision.
- The proxy restarts only when its own code has changed. A short restart during a run is survivable, by the measurement above.

## Settled later

- The core is not started from a checkout but from a program built in a sealed container (ADR 0036, ADR 0037).
