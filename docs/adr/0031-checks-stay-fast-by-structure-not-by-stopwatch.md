---
status: accepted
---

# Checks on Kiln's own repo stay fast by structure, not by a stopwatch

Kiln's own checks run on the developer's machine (ADR 0026), where elapsed time measures how busy the machine is as much as how heavy the tests are. So nothing fails because a whole check run took too long. Yet if nothing fails when a card makes the tests slower, they get slower card by card, and a time budget on the whole run fails on load noise.

1. **Tests are sized by what they may do.** Small tests run in one process with no network, no sleeping and no other programs. Anything else is a large test and is marked as one.
2. **Per-test limits with wide margins.** A small test that normally takes milliseconds fails at around a second; a busy machine does not move a test by a factor of a hundred.
3. **Unchanged code is not re-tested.** A card pays for what it touches.
4. **CPU time is recorded per check run, not elapsed time, and fails nothing yet.** Kiln shows the trend. If it climbs, a budget on CPU time can be added as a decision, on a measure a busy machine barely affects.
5. **No blanket retries.** A failing test is re-run only if it is on an explicit list of unreliable tests, and only the developer can add to that list, so an agent cannot excuse its own failing test.

This follows how others handle it: Google sizes tests by what they may do and denies small tests the main sources of slowness; Bazel gives each size a limit with a wide margin; SQLite measures CPU cycles because wall-clock times are "scarcely repeatable beyond one significant digit"; Go's test tool does not re-run packages whose inputs are unchanged. No established practice of failing a build on total suite time was found.

## Considered options

- **A time budget for the whole check run**: rejected; it fails for the wrong reason on a busy machine.
- **Comparing against the base in the same run**: rejected; it doubles every check run on the machine that is already the bottleneck.
- **Trend and alert only**: not enough alone, since an alert stops nothing; rules 1 to 3 and 5 are what is added to it.

## Consequences

- The limits, the marking of large tests and the list of unreliable tests live in the check configuration, which is a protected path (ADR 0006).

## Settled later

- The checks are one command in the repo, and these rules live in it (ADR 0045).
- The checks now run on the Git host's own runners and not on the developer's machine (ADR 0049). The rules stand, since those machines vary in speed as well.
