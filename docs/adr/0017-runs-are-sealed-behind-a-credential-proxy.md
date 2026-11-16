---
status: accepted
---

# Every run is sealed, and credentials stay behind a proxy

Runs execute unattended and without permission prompts, so each one runs in a sealed container from the first day. Claude Code's own sandbox covers shell commands only and can still read most of the machine, and its documentation says sessions that skip permission prompts belong inside a container, a VM or the sandbox runtime.

A sealed run has:

- no network except a proxy outside the container, which allows only listed hosts;
- nothing mounted from the developer's home directory;
- no credentials inside, for Claude or for git; Kiln publishes, the agent does not;
- memory and CPU limits;
- its code from the instance's own copy at a known commit, and its results out only through save (ADR 0013).

The proxy adds the Claude credential to requests and renews it in one place, so agents never hold a token and never renew one themselves. In the developer's setup the credential comes from a single browser sign-in and is renewed silently afterwards, so the proxy needs the renewable secret, not the developer's presence. Having each run ask a process on the developer's machine for tokens would fail in the middle of runs whenever that process is unreachable.

Because the agent never pushes, the only thing that writes to a Git host is Kiln's own publishing. Kiln pushes only to branches that match the target repo's card branch pattern (ADR 0013) and never to any other branch, whatever the host would allow. Anthropic's self-hosted Managed Agents server restricts its agents' pushes to listed branch prefixes in the same way (ADR 0002).

## Consequences

- A run relies on nothing on the machine it happens to run on, which is what lets a second runtime be added later without rework (ADR 0018).
- How the proxy obtains a credential is specific to a setup and is kept in one place; Kiln itself only knows "something that yields a credential" (ADR 0003).
- Putting the renewable secret anywhere but the developer's own machine is a decision about who else can then act as the developer.

## Settled later

- Where the proxy sits: a container of its own on the runs' network (ADR 0034).
- "Results out only through save" was too narrow for a spec or a verdict. Results leave a run only through what the core collects after it has ended (ADR 0038).
- The proxy does not hold the renewable secret. The core runs a command that yields a credential and pushes the result to the proxy (ADR 0040).
- How a network is sealed on the developer's container engine (ADR 0041).
- The checks runner is not covered by this decision; it is isolated, not sealed (ADR 0046).
