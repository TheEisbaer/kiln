---
status: accepted
---

# The core pushes the Claude credential to the proxy; the renewable secret never enters a container

The proxy is a container (ADR 0034), while whatever yields the developer's Claude credential runs on their machine. The instance therefore names one command that prints a credential. The core runs it and hands the result to the proxy through the container engine, into the proxy's container, regularly and whenever the proxy reports that the credential was refused. A command that fails means the credential is unavailable, and the instance pauses (ADR 0016).

This is all Kiln knows about a setup's sign-in (ADR 0017, ADR 0024): a command that prints a credential, the same contract Claude Code has for its own key helper. In the developer's setup the existing helper already is such a command; it renews silently and fails without a terminal when a browser sign-in would be needed.

ADR 0017 says the proxy "needs the renewable secret". It does not: the proxy holds only a credential that expires within about an hour, and renewal stays in one place, the command on the developer's machine. That is stricter on ADR 0017's own last consequence, about where the renewable secret may be.

## Considered options

- **The proxy asks a service on the developer's machine when it needs a token**: it fails in the middle of runs whenever that service is unreachable (ADR 0017).
- **The proxy runs the helper itself**, with the helper, its runtime and the secret's file mounted into it: the proxy would need nobody. Rejected because the renewable secret would sit in a container next to the runs, so a fault in the proxy would expose months of access and not one hour; a setup's own helper would have to run inside Kiln's image; and one file would have two writers across the boundary of the engine's virtual machine.
- **The proxy renews by itself from a sign-in configuration**: Kiln would have to know a sign-in protocol.

## Consequences

- Renewal needs the core. If the core is down for minutes exactly when the credential expires, runs in flight fail and are retried once (ADR 0016). Measured on 2026-11-14 and 2026-11-16: Claude Code retries a refused credential and a refused connection ten times over two to three minutes, and a restart of the core takes seconds.
- A refused credential shows in a run's output within about a second, and it differs there from a lost connection and from any other failure. The core can therefore tell a failure that is about the instance (ADR 0016) from one that is about a run, and need not wait for the retries to run out.
- On Kubernetes the command runs wherever the core runs, which puts the renewable secret in the cluster. That is the decision ADR 0017 names, and it is made when Kubernetes arrives.
