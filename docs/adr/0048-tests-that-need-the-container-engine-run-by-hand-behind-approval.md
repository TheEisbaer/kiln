---
status: superseded in part by ADR 0049
---

# Tests that need the container engine are run by hand, behind the developer's approval

> Revised by ADR 0049: with a public repo the checks run on the host's own runners, each with a container engine, so these tests run in the checks. Only tests that need the developer's Claude credential are still run by hand. The code that defines the seal stays a protected place.

Kiln's riskiest code starts containers: the seal, the proxy, starting a run and collecting from it. Tests of that code need a container engine, and neither place where checks run has one: a sealed run by ADR 0017, the checks runner by ADR 0046. Such tests are therefore large tests (ADR 0031) that the check command skips on the runner, and one command runs them on the developer's machine. The code that defines the seal and starts runs sits in a protected place (ADR 0047), so a change to it always waits for the developer, who runs these tests before approving.

Elsewhere such tests run automatically because every job gets an engine: hosted runners provide one inside a throwaway virtual machine, and where the job itself runs in a container the common pattern is to mount the host engine's socket into it. On the developer's machine that socket is not harmless. The engine's virtual machine shares the developer's home directory and hosts their work cluster, so code holding the socket could reach both, and the proxy's credential as well.

## Considered options

- **Mounting the engine's socket into the runner**: automatic and common, but it undoes ADR 0046.
- **A second virtual machine only for Kiln, with nothing mounted, whose engine the runner may use**: automatic, and it would also separate Kiln from the work cluster. Not now: it costs a second machine's memory and CPU, checked code could still read the proxy's credential there, and nothing yet shows it is needed. It is the answer if the history shows these changes arriving often, or failing runs traced to untested changes of the run path.
- **The core testing its own seal at every start**: catches a broken seal on the real machine, but only after the merge, and it tests nothing else of the run path.

## Consequences

- Every card that touches this code costs an intervention (ADR 0007). That is accepted because the seal is what the developer's attention is best spent on.
- A regression in the run path outside the seal shows as failing runs and reaches the developer by the usual rules (ADR 0016).

## Settled later

- Creating a run's network and container, and starting the proxy from the credential command, are functions of `internal/seal`. The caller gives no network, mount, environment or limit. What a run can reach, read and obtain is asserted from inside a run's container in `internal/seal`'s test.
