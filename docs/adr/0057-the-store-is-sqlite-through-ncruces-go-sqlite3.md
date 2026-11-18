---
status: accepted
---

# The store is SQLite, through ncruces/go-sqlite3, and the modules come in the toolchain image

ADR 0032 chose one embedded database file, ADR 0037 limited the choice to code written in Go, and ADR 0051, rule 4, asks for an ADR when the database is chosen. The first card that keeps a card's state and history (`internal/store`) chose it, on what was measured on 2026-11-18.

The store is a SQLite file, opened through `github.com/ncruces/go-sqlite3` and `database/sql`, with one connection. The library is SQLite 3.54.1 translated to Go, with its file access written in Go, so the core still has no C code. SQLite's defaults stay: a rollback journal, which leaves one file when the store is closed, and a sync of every commit; on macOS the library syncs with `F_FULLFSYNC`. The schema's version is in the file (`PRAGMA user_version`) from the first store, and a core refuses a newer one (ADR 0036). macOS's own `sqlite3` reads the file, and `.dump` is the readable export that ADR 0032, rule 2, asks for.

Because the checks run with the network off (ADR 0045), the module and the three it needs are downloaded when the toolchain image is built, from `go.mod` and `go.sum`, and the image then turns the module proxy off. A module the image lacks fails with "module lookup disabled", and the run image, built on the toolchain image, has them too. The hashes in `go.sum` are checked when they are downloaded.

## Considered options

- **`modernc.org/sqlite`**: SQLite transpiled from C, the more proven of the two. Ten modules, one of which must be pinned exactly, about twice the compile time (28 s against 14 s with 4 CPUs) and 302 MB downloaded. The same file through `database/sql`, so it can replace the chosen one cheaply.
- **`go.etcd.io/bbolt`**: a key-value store, the smallest and the fastest to build. It has no SQL, so the measures of ADR 0007 would be loops in Go, and a readable export would be code of Kiln's own.
- **One JSON file, rewritten atomically**: no dependency. Every change rewrites the whole history, and the measures and the migrations of ADR 0036 would be Kiln's own code.
- **The modules in `vendor/`**: nothing to build into the image, but 390,000 lines in the repo and in every update's diff, which the scan of ADR 0050 reads too. The `go` tool does not check `vendor/` against `go.sum`.

## Consequences

- The library is version 0, and its file access is a rewrite that its own README calls "not as battle tested as the original". Kiln updates it only by a card (ADR 0023).
- A change to `go.mod` needs a rebuilt toolchain image and run image before a run or a build can use the new module. A sealed run cannot add a module, because it has no network.
