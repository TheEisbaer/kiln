---
status: accepted
---

# Kubernetes comes later; the MVP's colleague installs Kiln on their own machine

ADR 0018 made Kubernetes (k3s) Kiln's second runtime, arriving at the MVP when a colleague needs to install Kiln, and asked that before Kubernetes is relied on, the whole path of a card is tested on it automatically. The MVP's sixth item asked for both. ADR 0018 also gave the reason that now speaks against it: of what the developer wants Kubernetes for, containers already give resource limits and isolation, and one deployment recipe on any machine "is not needed until an instance has to run somewhere else".

The MVP's colleague does not need it. They run Kiln on their own Mac with the same container engine as the developer, the same setup that ADR 0003 names, so every MVP item is reached on the container engine the POC was built on. A second runtime for an instance that runs where the first one runs would be built ahead of need, which ADR 0003 rules out. The developer decided this on 2026-11-26.

1. **The MVP's colleague installs Kiln on their own Mac**, with the same container engine as the developer, from the instructions alone. A fresh install from those instructions passes an automated test.
2. **Kubernetes (k3s) is not part of the MVP.** It is not built at the MVP, and the whole path of a card is not tested on it there. It is on the "Later" list of `docs/phases.md`.
3. **It comes back as its own decision** once an instance has to run somewhere else than a person's own machine: a shared server, or a machine without a container engine.
4. **What ADR 0018 asked for to keep that move possible stays.** Runs stay sealed (ADR 0017), and nothing in Kiln may depend on Kiln and its runs sharing a filesystem.

## What this changes in earlier decisions

- **ADR 0018:** "Kubernetes (k3s) is the intended second runtime and arrives at the MVP, when a colleague needs to install Kiln"; "before Kubernetes is relied on, the whole path of a card is tested on it automatically", as an MVP item; and "What Kubernetes adds beyond containers (chart, image distribution, network policy, secret delivery) is built at the MVP". Kubernetes is the second runtime later, when rule 3 says, and the test on it and what it adds come with it. The rest holds: the POC on the developer's machine, runs sealed from the first day, and no filesystem shared between Kiln and its runs.
- **ADR 0038, consequences:** "Placing and collecting files is the one place that is rebuilt at the MVP, where the whole path of a card is tested on k3s in any case." It is rebuilt when Kubernetes arrives, which is no longer at the MVP.
- **ADR 0023:** "Kubernetes, which come at the MVP (ADR 0018)". Kubernetes comes later. The choice of Go stands.
- **`docs/phases.md`, MVP item 6**, which this decision changes as ADR 0004, rule 3, allows. The item now asks for the colleague's install on their own Mac and for an automated test of a fresh install, in place of the test on k3s.

## Considered options

- **k3s at the MVP, as ADR 0018 wrote it**: the colleague installs on k3s, and the whole path of a card is tested there automatically. It brings the one recipe for any machine sooner. The price is the work ADR 0018 names (chart, image distribution, network policy, secret delivery), placing and collecting rebuilt (ADR 0038), and an update per merge that becomes an image build and rollout (ADR 0019), all for a colleague whose machine already runs the container engine.
- **Build k3s before the colleague tries**: the colleague's first install is on the runtime meant to last, with no second move. It holds the MVP's colleague behind all of that work, and fixes the cluster's shape before anyone has run Kiln on a second machine.

## Consequences

- An instance runs only where a container engine runs, on a person's own machine. A shared server, or a machine without a container engine, waits for the decision of rule 3.
- The instructions for a new install, and an automated test of a fresh install from them, are MVP work. They replace the test of a card's whole path on k3s.
- The sentences that say "when Kubernetes arrives" (ADR 0017, ADR 0019, ADR 0036, ADR 0040) still hold. Kubernetes arrives later.

## Revisit when

- An instance has to run somewhere else than a person's own machine.
- A colleague's machine cannot run the developer's container engine.
