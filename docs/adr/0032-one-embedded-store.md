---
status: accepted
---

# Kiln keeps its data in one embedded database file

Everything Kiln knows about cards and runs lives in a single embedded database file in the instance's data directory. There is no database server to run, back up or lose; changes are atomic; the counts Kiln is judged by (ADR 0007) are simple queries; a backup is a copy of one file; and on Kubernetes it is a file on a volume. Two stores side by side end up disagreeing, and a database server is one more thing whose upgrade can fail and take the data with it.

1. **One store.** The only things outside it are the raw output files of runs, which are large; the store points to them.
2. **It can always be exported as readable text,** so inspecting the data never requires Kiln to be running.

## Considered options

- **Plain files, one folder per card**: readable and easy for a person or an agent to inspect, but without atomic changes records drift, and every count means reading everything.
- **A database server**: scales to many users and machines, which one developer per instance (ADR 0001) does not need.

## Consequences

- One instance writes to the store at a time, which matches one instance per developer.
- The instance's own copies of target repos (ADR 0013) are git repositories on disk and are not part of the store.

## Settled later

- The file is SQLite, and `sqlite3 <file> .dump` is the readable export of rule 2 (ADR 0057).
- The schema is version 2: besides cards and their entries, an `instance` table with one row, the instance's identity of eight hex digits, and a `runs` table with one row per run, with a run's ending empty while it is in flight. A run's row is kept for good, so that the measures can count it (ADR 0029, ADR 0030); the files of its record stay until its card is done or dropped (ADR 0053), and are removed by the card that first ends a card.
