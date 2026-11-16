---
status: accepted
---

# A sealed network has no gateway and holds one run

"No network except a proxy" (ADR 0017) is not what the container engine's internal networks give by themselves. Measured on the developer's container engine on 2026-11-14, where the engine's virtual machine also hosts a Kubernetes cluster with the developer's work applications: a container on a plain internal network could not reach the internet, the developer's machine or the cluster's pods, but through the network's gateway address it reached the virtual machine's own services, among them remote login, the cluster's API and a port published by a work application. Containers on the same network could also reach each other.

1. **A run's network is created without a gateway.** With the engine's isolated gateway mode, nothing but other containers on the same network was reachable.
2. **Every run gets a network of its own,** shared only with the proxy. Another run is a host other than the proxy, and a sealed run reaches nothing Kiln has not handed it.
3. **Claude Code's non-essential traffic is switched off in a run.** Left on, it tries to reach its maker's servers for flags and telemetry regardless of the configured address; sealed, that fails quickly and harmlessly, but it is noise in every run.

4. **The proxy container has packet forwarding switched off.** A container on two networks can otherwise be used as a router. Measured on 2026-11-16 in a temporary virtual machine: with forwarding left on, and without a kernel module that the cluster on the developer's virtual machine happens to load, packets from a run that pointed its route at the proxy arrived at a container on the ordinary network, at the virtual machine and left it towards the internet, though no reply came back. With forwarding switched off nothing passed the proxy, with or without that module.

## Consequences

- The proxy is attached to one more network per run in flight, and detached when the run's container is removed.
- The seal must not rest on what else happens to run on the same virtual machine. The POC's own test of the seal (a run cannot reach a host other than the proxy) is what decides on any given machine.
