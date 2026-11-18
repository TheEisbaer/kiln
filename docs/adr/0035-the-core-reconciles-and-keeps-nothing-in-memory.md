---
status: accepted
---

# The core keeps nothing in memory that matters; it reconciles the store, the container engine and the Git host

While the core is down, runs finish, pull requests merge and ceilings pass, so the store alone is never the whole truth after a restart. Each fact therefore has one owner: the store holds what Kiln decided, the container engine holds which runs exist, whether they are alive, how they ended and what they wrote, and the Git host holds the state of pull requests. On start and on every tick the core compares the three and acts on the difference. A restart is then the normal path, exercised every few seconds, and not a special one that is first tried when an update goes wrong.

1. The core can be killed at any moment and loses nothing.
2. A run's container carries its run's identifier, so the core finds it again without remembering it.
3. A finished container stays until its result is in the store and its work is saved (ADR 0013); only then is it removed.
4. A mismatch has a fixed answer. A run in the store with no container is a failed run (ADR 0016). A container with no run in the store is removed.
5. The time ceilings (ADR 0029) are checked from the engine's facts, a run's start time and the time of its last output, so they hold across a restart.
6. Events from the engine, and anything pushed to the core, are hints that may shorten the wait for the next tick. They never replace it.

## Considered options

- **The store alone, trusted on start**: one source, but whatever happened during the downtime is missing from it, so catching up is needed anyway, as a separate path.
- **Events with state in memory and a journal replayed after a restart**: reacts at once, but replay is a rarely used path whose faults show exactly when an update fails.

## Consequences

- Every step the core takes must be safe to repeat.
- Kiln reacts with a delay of up to one tick.

## Settled later

- A run's work is saved, then the run is recorded, and only then is its container removed (rule 3). The same command started again is a repeat of the run and not a retry: each step looks at what is there and goes on from it, so a repeat uses up nothing of ADR 0016.
