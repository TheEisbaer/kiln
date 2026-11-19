# Settled by cards

What cards have settled under a decision, by the decision's number. A card adds its lines here, in the pull request that settles them, and never changes a file in `docs/adr/`. The decisions themselves change only by the developer (ADR 0058).

A line says what was settled and, where it matters, when and by which card. The spec that a line comes from is the text of that card's pull request.

## ADR 0005: Kiln's work ends at "ready to merge"; merging belongs to the target repo's Git host

- Where everything the Git host requires has already passed, the Git host refuses to switch on automatic merge, and Kiln asks the Git host to merge instead (ADR 0049). The Git host's rules decide in both cases; Kiln never merges past them.

## ADR 0006: Guardrails for automatic merging

- Checks now run on the Git host's own runners, not on a runner on the developer's machine (ADR 0049).
- The developer's approval of a protected path is given on the Git host. It is an intervention (ADR 0042), which the core records when it sees the approval there.
- Kiln's review instructions, with all its stage instructions, are a protected place in Kiln's repo (ADR 0056).

## ADR 0012: A moving repo sends a card back to implement; a spec that cannot be built sends it back to spec

- A card that returns to implement from the Git host counts as a send-back, for a conflict as for a failed check (ADR 0054).
- A card that returns to spec has its spec session resumed, since that stage gets its own work back; after the new approval implement starts in a fresh session and the number of send-backs starts again (ADR 0053, ADR 0054). That approval is not the planned one and is the intervention counted here (ADR 0042). How a run reports that the spec cannot be built is listed in docs/open-points.md.

## ADR 0013: Work is saved after every implement round and published only when review has passed

- Runs reach nothing. The core places the code in a run and collects the change from it through the container engine (ADR 0038), so "reachable from them" now means reachable from the core.

## ADR 0015: Review blocks only on the spec, a concrete failure or a written standard

- "Not written anywhere" means that remarks are no part of review's result and are shown to nobody. The full output of a review run is still kept, like any run's (ADR 0032, ADR 0038).
- Every review round starts in a fresh session; a stage that is sent back resumes its own (ADR 0053).
- The number of send-backs is a fact on the card, and a failed check on the Git host counts as one (ADR 0054).

## ADR 0016: Run failures are handled by a few fixed rules, not by a supervising agent

- Whether a run has been retried is a fact on the card (ADR 0054).
- That the instance is paused is state of the instance, kept in the store with its own history (ADR 0055).
- Until the retry card, a failed run is not retried: it moves its card to "needs you", in spec waiting for the developer, with the run's reason on the entry that ends it. Finished and failed are told apart by the run's record (ADR 0043). A missing result and a kill for memory are still the retry card's (card 8, 2026-11-18).

## ADR 0017: Every run is sealed, and credentials stay behind a proxy

- Where the proxy sits: a container of its own on the runs' network (ADR 0034).
- "Results out only through save" was too narrow for a spec or a verdict. Results leave a run only through what the core collects after it has ended (ADR 0038).
- The proxy does not hold the renewable secret. The core runs a command that yields a credential and pushes the result to the proxy (ADR 0040).
- How a network is sealed on the developer's container engine (ADR 0041).
- The checks runner is not covered by this decision; it is isolated, not sealed (ADR 0046).
- "A second runtime later without rework" was given up for one place: placing files in a run and collecting them from it is rebuilt for Kubernetes (ADR 0038).
- The proxy lets a request's body be read while its answer is passed on (`EnableFullDuplex`), so that Go's HTTP/1 server does not close the body at the answer's first write. What a run can reach or obtain does not change (card 10, 2026-11-18).

## ADR 0018: The POC runs on the developer's machine; Kubernetes is the second runtime

- A run is not quite "a pod in all but name": placing files in a run and collecting them from it uses the container engine and is rebuilt for Kubernetes at the MVP (ADR 0038).
- Kiln no longer runs from a checkout. Each version is a program built in a sealed container, with the previous one kept beside it (ADR 0036, ADR 0037).

## ADR 0019: Kiln updates itself after a merge to its own repo

- Which process an update restarts, and why runs do not notice (ADR 0034, ADR 0035).
- Who judges a new version and undoes a failed update, and what happens to the store when it is undone (ADR 0036).
- Where the new version is built (ADR 0037).
- Kiln no longer runs from a checkout. Each version is a program built in a sealed container, with the previous one kept beside it (ADR 0036, ADR 0037).
- That automatic merging is off after a failed update is state of the instance (ADR 0055).

## ADR 0020: Stage runs use Claude Code's headless command line, and nothing calls a model outside a run

- Which part of a run's image is Kiln's, and so where the pinned Claude Code version lives (ADR 0039).
- How code and instructions get into a run and results out of it (ADR 0038).
- A run's command line is `sh -c 'exec claude "$@" < /kiln/instruction.md' sh -p --output-format stream-json --verbose --permission-mode bypassPermissions --model <id> --effort <level>`, without `--bare`, so that the repo's `CLAUDE.md` reaches the prompt. The instruction comes on standard input.

## ADR 0022: The UI shows what needs the developer first; the board is second

- The first screen shows everything that waits for the developer: specs to approve, drops to confirm, and cards in the state "needs you". The term itself keeps its narrower meaning from CONTEXT.md, a card that has stopped outside the planned approval.

## ADR 0025: Kiln's settings for a repo live in the instance; the repo's own Claude Code configuration is picked up from the repo

- Protected paths and the required checks are not settings in the instance. For a repo that merges automatically the Git host enforces both, from the code owner file in the repo and from its own rules (ADR 0047, ADR 0049), and the checks are a command in the repo (ADR 0045). Each still has one place. The instance holds what a Git host cannot: whether automatic merging is used, the limit, branch naming and the toolchain image (ADR 0039).

## ADR 0026: Kiln's repo lives in the developer's personal space on their company's GitHub; checks run on the developer's machine

- How the checks are defined (ADR 0045).
- How the runner is protected: isolated like a hosted runner, not sealed like a run (ADR 0046).
- Where tests run that need the container engine, which the runner does not have (ADR 0048).
- Kiln cannot act as an app on a personal repo on this Git host, so the fallback this decision names was taken: a public repo on github.com under the developer's personal account, with checks on the Git host's own runners (ADR 0049). GitHub as the first Git host, Kiln building no checker, and Kiln acting under its own identity all stand.
- Of this decision's cost rules, two stand: Kiln's repo causes no billable usage, and there are no hosted background jobs. The rules about the developer's own runner and its caches ended with that runner (ADR 0049). What may be stored on the Git host, and how CPU time per check run is recorded there (ADR 0031), is settled by the card that needs it.

## ADR 0030: Opus for spec and review, a model proposed per card for implement, Fable only as an advisor

- That a send-back happened is read from the number of send-backs on the card (ADR 0054). A session that is resumed after a send-back continues on the stronger model (ADR 0053).
- The core compares the model that a run's first event, `system/init`, names with the one it gave. A run on another model is stopped, saved and recorded with the model seen, and the command exits non-zero. Claude Code 2.1.326's output does not show the effort, so the effort recorded is the one given.

## ADR 0031: Checks on Kiln's own repo stay fast by structure, not by a stopwatch

- The checks are one command in the repo, and these rules live in it (ADR 0045).
- The checks now run on the Git host's own runners and not on the developer's machine (ADR 0049). The rules stand, since those machines vary in speed as well.

## ADR 0032: Kiln keeps its data in one embedded database file

- The file is SQLite, and `sqlite3 <file> .dump` is the readable export of rule 2 (ADR 0057).
- The schema is version 2: besides cards and their entries, an `instance` table with one row, the instance's identity of eight hex digits, and a `runs` table with one row per run, with a run's ending empty while it is in flight. A run's row is kept for good, so that the measures can count it (ADR 0029, ADR 0030); the files of its record stay until its card is done or dropped (ADR 0053), and are removed by the card that first ends a card.

## ADR 0034: Kiln is a core on the developer's machine, a proxy in a container, and one detached container per run

> Since ADR 0059 a run with a list of hosts has a way out, a fourth kind of process. The lines below still hold.

- The core is not started from a checkout but from a program built in a sealed container (ADR 0036, ADR 0037).

## ADR 0035: The core keeps nothing in memory that matters; it reconciles the store, the container engine and the Git host

- A run's work is saved, then the run is recorded, and only then is its container removed (rule 3). The same command started again is a repeat of the run and not a retry: each step looks at what is there and goes on from it, so a repeat uses up nothing of ADR 0016.
- One core at a time holds a data directory: `kiln core` takes `flock` on `<dir>/lock` for the life of its process, and a second core, or `kiln card`, is refused. The kernel drops the lock with the process, also when a child such as `docker logs` lives on. The store's unique index refuses a second run in flight per card as well, since a lock alone does not guarantee one holder (card 8, 2026-11-18).
- The engine's things are known as an instance's by the label `kiln.instance` with the instance's eight hex digits, beside `kiln.run`, on a run's network and container (rule 2). Every tick sweeps first: what carries both labels and is not in flight in the store is removed (rule 4). Nothing without both labels is touched, and `kiln run` by hand sets no instance, so no core lists what it made (card 8).
- A run is written to the store between placing its container and starting it, so that a kill before leaves a container that never started, which the sweep removes, and a kill after leaves a run whose files are placed again into the container that is there, which is then started once.
- From the moment a run is in the store until its result is recorded, nothing in the core removes its container or network, and its container is never created again. A step that fails, an interrupt or a kill in between changes nothing, and the next tick goes on from what is there. Only the engine losing a container makes a run gone: one with no container and nothing recorded is failed (rule 4) (by the developer's decision of 2026-11-19, after card 8's second review).
- At a run's end the work is saved, then `result.json` is written, then the store, and only then are the container and network removed (rule 3) (card 8).
- A run's ID is `<instance>-<card>-<number of the card's runs + 1>`. It is the same on every tick until the run is in the store, and the engine allows one container per name, so a wait gets one run (card 8).

## ADR 0037: Kiln is built in a sealed container, and the core contains no C code

- The database is pure Go: SQLite translated to Go, through `ncruces/go-sqlite3` (ADR 0057).

## ADR 0038: The container engine carries everything into and out of a run

> Since ADR 0059 a run with a list of hosts also reaches the hosts on its list. The lines below still hold.

- What is placed in a run, and how: the commit's files under `/work`, owned by the run's user, and the instruction at `/kiln/instruction.md`. The seal writes them as one archive into a container that has never started, and refuses absolute names, `..` and anything but files, directories and symlinks, so nothing else can be placed.
- The change is collected by saving the working tree in a throwaway container from the run image, with no network, the run's user and limits, and no mounts. The engine pipes the tree from the run's container into it, so nothing of it is unpacked on the developer's machine; what comes back is a tree and a pack of its new objects.
- A stage's documents and the run's sessions are kept as the archives the engine hands over, `documents.tar` and `session.tar` in the run's record, and are never unpacked on the developer's machine.

## ADR 0039: A run image is a target repo's toolchain image with Kiln's layer on top

- Kiln's layer is `images/run/`. Claude Code is downloaded from the release bucket and checked against a SHA-256 per platform, and a run is user `kiln`, uid 1000, because Claude Code refuses `bypassPermissions` as root.

## ADR 0040: The core pushes the Claude credential to the proxy; the renewable secret never enters a container

- Whether runs that lose the credential while the core is down use up a card's retry, or pause the instance as rule 3 of ADR 0016 says for a failure of the instance, is listed in docs/open-points.md.

## ADR 0041: A sealed network has no gateway and holds one run

> Since ADR 0059 a run with a list of hosts shares its network with its way out as well. The lines below still hold; where one says that a run reaches the proxy and nothing else, it speaks of a run without a list.

- This decision's measurements were IPv4 only. On 2026-11-17 a run network with IPv6 off turned out to keep IPv6 on its bridge on the virtual machine, with a link-local address; a container with Docker's default capabilities has `NET_RAW`, sent raw frames to it, and reached the virtual machine's remote login and a published port. A run network is now closed in both families: IPv6 on, with the gateway mode isolated for IPv4 and IPv6, which leaves the bridge with IPv6 off and no address. The proxy has forwarding off in both families, and the seal's test checks both.
- As a non-root user a run has no effective capabilities whatever is dropped, and the bounding set matters only through setuid and file-capability programs. A run's container drops all capabilities and sets `no-new-privileges`, by the developer's decision of 2026-11-17.
- A run's network carries the label `kiln.run` with the run's ID, as its container does. The seal removes or attaches to a network of a run's name only with that label, and refuses one without it. A proxy whose container does not exist is not attached, so a run's network is removed without it, by the developer's decision of 2026-11-17.
- A run's network is created, and attached to, only for a proxy that carries the label `kiln.proxy`. One without it is refused before anything is created and left untouched, so that a mistyped proxy does not put another container on a run's network, by the developer's decision of 2026-11-17.

## ADR 0042: A card's current state and its history are written together

- A card carries two more facts beside its stage and what it waits for, so that no rule has to read the history (ADR 0054).
- The one place is `change` in `internal/store`: one transaction that checks, writes the card and writes its entry (ADR 0057).

## ADR 0043: A card stores its stage and what it waits for; a run is in flight or has ended in one of four ways

- "The developer's approval" covers confirming a proposed drop, since a spec run ends with one of two results (ADR 0011).
- A card carries two more facts beside the two lists: its number of send-backs and whether its run was retried (ADR 0054).
- `change` in `internal/store` is the one place of rule 4, and the 11 valid pairings are one table in its code (ADR 0057).
- A run's ending is read off its record: finished when its container exited with 0, the result is `success`, the model is the one asked for, and the work is saved or nothing changed; failed otherwise. Until the retry card a failed run moves its card to "needs you" (ADR 0016). Until the stage cards a card waits in spec for a run, which is told the card's text; a finished run moves it to wait for the developer's approval (card 8, 2026-11-18).

## ADR 0044: The UI sends requests; the core tells it about changes over one one-way event stream

- The stream also carries the entries of the instance, such as a pause (ADR 0055).
- Until the API comes, `kiln card --data <dir> <text>` stands in for it: it writes a card that waits in spec for a run. It takes the core's lock, so it never writes beside a running core (rule 5). The API card removes it in the pull request that adds the API (card 8, 2026-11-18).

## ADR 0045: Kiln's checks are one command that runs the tools directly in the toolchain image

- "The same everywhere" holds for what the check command can run inside the toolchain image. Tests that need the container engine run in the checks on the Git host only (ADR 0049); how they are started there is settled by the first card that has such a test.
- The toolchain image carries the Go modules of `go.mod` and `go.sum`, downloaded when it is built, so that the check finds them with the network off (ADR 0057).

## ADR 0047: One repo with one Go module, divided by Kiln's parts, with guardrail files in a few named places

> Since ADR 0059 the way out is a part as well, and its program is a place of the seal. The lines below still hold.

- One more named place: Kiln's own stage instructions, in `instructions/` (ADR 0056).
- The places of the seal, which ADR 0048 names as protected, are `cmd/proxy/` and `internal/seal/`; the image of the proxy is under `images/`.
- Only `internal/seal` runs the `docker` command. A step of the check command fails when a Go file outside it that is not a test contains the string literal `"docker"`; it stops a slip, not an evasion.
- The core is `cmd/core`, built as `kiln`, and the steps of a run are `internal/run`, not `internal/core`: a package named after the whole core would take everything. Both call `internal/seal` and do not run the `docker` command.
- The step of the check command also fails on a string literal whose first word is `docker`, as in a shell line, or that is a path ending in `/docker`.

## ADR 0048: Tests that need the container engine are run by hand, behind the developer's approval

- Creating a run's network and container, and starting the proxy from the credential command, are functions of `internal/seal`. The caller gives no network, mount, environment or limit. What a run can reach, read and obtain is asserted from inside a run's container in `internal/seal`'s test.

## ADR 0049: Kiln's repo is public on GitHub, under the developer's personal account

- The engine tests are started by `check/in-image`: always on the Git host's runner, and on the developer's machine only with `--engine`, so that a test built from a card's branch does not run there unasked (ADR 0048). They are compiled in the toolchain image with the network off and run on the machine that runs the check, since the image has no engine.
