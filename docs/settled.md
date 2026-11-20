# Settled by cards

What cards have settled under a decision, by the decision's number. A card adds its lines here, in the pull request that settles them, and never changes a file in `docs/adr/`. The decisions themselves change only by the developer (ADR 0058).

A line says what was settled and, where it matters, when and by which card. The spec that a line comes from is the text of that card's pull request.

## ADR 0005: Kiln's work ends at "ready to merge"; merging belongs to the target repo's Git host

- Where everything the Git host requires has already passed, the Git host refuses to switch on automatic merge, and Kiln asks the Git host to merge instead (ADR 0049). The Git host's rules decide in both cases; Kiln never merges past them.

## ADR 0006: Guardrails for automatic merging

- Checks now run on the Git host's own runners, not on a runner on the developer's machine (ADR 0049).
- The developer's approval of a protected path is given on the Git host. It is an intervention (ADR 0042), which the core records when it sees the approval there.
- Kiln's review instructions, with all its stage instructions, are a protected place in Kiln's repo (ADR 0056).

## ADR 0008: One planned intervention per card: approval before any code is written

- The developer approves a spec with a move on the screen: it moves the card from spec, waiting for approval, to implement, waiting for a run. The developer's moves are one table in the store (`Decide` in `internal/store`): approve, again, send back and done (card 13, 2026-11-19). Done is the move from review, waiting for approval, where a review that passed leaves the card; ready to merge comes with publishing (card 14, 2026-11-19).
- The developer's moves carry a note, at most 16 KiB, trimmed, kept as the reason of the move's entry; no column is added. A spec that waits for approval is sent back to a spec run with a note it cannot go without (`again`), a review that passed is sent back to implement the same way (`send-back`), the moves of a stage that needs you take one or none, and approve and done refuse one. The run that follows a move with a note is told it last, under `## The developer's note`, and the note wins where it differs from the spec (card 16, 2026-11-20).

## ADR 0011: A card ends as done or dropped, and only the developer drops it

- The developer drops a card with a move on the screen, `drop`, from every state but done, with one confirmation that names the card and an optional note. A card with a run in flight is not dropped (409) until stopping a run comes. The branch of a dropped card stays (card 16, 2026-11-20).

## ADR 0012: A moving repo sends a card back to implement; a spec that cannot be built sends it back to spec

- A card that returns to implement from the Git host counts as a send-back, for a conflict as for a failed check (ADR 0054).
- A card that returns to spec has its spec session resumed, since that stage gets its own work back; after the new approval implement starts in a fresh session and the number of send-backs starts again (ADR 0053, ADR 0054). That approval is not the planned one and is the intervention counted here (ADR 0042). How a run reports that the spec cannot be built is listed in docs/open-points.md.
- Until a run can say that its spec cannot be built, the developer sends a card back to spec by hand: `to-spec`, from implement or review that needs you, to a spec run, with a note or without. That run starts fresh, not resumed: it is told the spec it replaces (`## The last spec`), the card's newest report or review (`## The last report` or `## The last review`), and the note, if any. A spec run after any note is told the spec it replaces as well (card 16, 2026-11-20).

## ADR 0013: Work is saved after every implement round and published only when review has passed

- Runs reach nothing. The core places the code in a run and collects the change from it through the container engine (ADR 0038), so "reachable from them" now means reachable from the core.
- A spec run's working tree is not saved. No save container, nothing taken into the instance's copy, no branch; `result.json` says `"saved": "not saved"` (card 11).
- An implement run's working tree is saved as one commit on `kiln/card-<id>`; a second implement run starts from the branch's tip. A review run starts from the tip as well, and its working tree is not saved (card 14, 2026-11-19).
- The save of an implement run has as its message the pull request title that the approved spec names on its line `` Pull request title: `…` ``, with `Kiln-Run: <id>` as its body. A spec that names none, or one that cannot be used, keeps `chore: save run <id>`. A repeat that finds the branch at this run's save with the same tree and another message counts it as saved (2026-11-20).

## ADR 0015: Review blocks only on the spec, a concrete failure or a written standard

- "Not written anywhere" means that remarks are no part of review's result and are shown to nobody. The full output of a review run is still kept, like any run's (ADR 0032, ADR 0038).
- Every review round starts in a fresh session; a stage that is sent back resumes its own (ADR 0053).
- The number of send-backs is a fact on the card, and a failed check on the Git host counts as one (ADR 0054).
- A review run is told the card, the approved spec and the diff, from where the card's branch left `main` to its tip, made in the instance's copy, and writes `review.md` with its verdict as the first line, `Verdict: passes` or `Verdict: goes back`. A diff over 1 MiB starts no review run, and the card needs the developer with the size. A change that goes back stops for the developer; nothing sends it back by itself yet. The next implement run is told the last review when it sent the change back (card 14, 2026-11-19).

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
- A run with a list of hosts reaches HTTPS on port 443 of the hosts on it through a way out of its own, and the way out's address rule (item 1 of card 12's spec) is decided on the address after the name is resolved: a name with one refused answer is refused whole, and the address judged is the one connected to. An empty list is the seal as before. Only spec runs have a list, `kiln core --spec-hosts`, and a spec run may read the web (the developer's decision of 2026-11-18, ADR 0009, ADR 0059) (card 12, 2026-11-19).

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
- A run with a list gets `HTTPS_PROXY` and `https_proxy`, which name its way out, and `NO_PROXY` and `no_proxy`, which name the proxy; `HTTP_PROXY` is not set. Claude Code 2.1.326 takes the first of these variables for all its traffic, the model's included, so without `NO_PROXY` its requests to `ANTHROPIC_BASE_URL` would go to the way out, with the placeholder key. The variables are a convenience: a repo's settings can change them and so break a run, never widen what it reaches, since the network decides (ADR 0059) (card 12, 2026-11-19).

## ADR 0021: A spec is one short document of Kiln's own, borrowed once from Spec Kit

- A spec run is told `instructions/spec.md` and the card's text, and writes one file, `/kiln/out/spec.md`, of at most 256 KiB. The core reads no line of it (card 11).
- The core reads one line of an approved spec: the first `` Pull request title: `…` `` before its first `## ` heading. The title is at most 256 bytes and has no backtick, control character or bidirectional control character (2026-11-20).

## ADR 0022: The UI shows what needs the developer first; the board is second

- The first screen shows everything that waits for the developer: specs to approve, drops to confirm, and cards in the state "needs you". The term itself keeps its narrower meaning from CONTEXT.md, a card that has stopped outside the planned approval.

## ADR 0023: Kiln's core is written in Go, its UI in Svelte

- The first screen is one embedded page, `internal/web/index.html`, with its CSS and JavaScript inside, and no build step, no dependency and nothing loaded from the network. A Svelte client replaces it when the screen outgrows one file (card 13, 2026-11-19).
- For work on the screen, `kiln core --page <file>` reads the page from that file on every request instead of the embedded one. Its Content-Security-Policy is computed from the file's own script and style, as the embedded page's is, so that the page runs as it will once embedded. A file that cannot be read, or that lacks a script or a style, is refused at start, and also refused without a screen (`--listen` empty). At a request it gives 500, never the embedded page (2026-11-20).

## ADR 0025: Kiln's settings for a repo live in the instance; the repo's own Claude Code configuration is picked up from the repo

- Protected paths and the required checks are not settings in the instance. For a repo that merges automatically the Git host enforces both, from the code owner file in the repo and from its own rules (ADR 0047, ADR 0049), and the checks are a command in the repo (ADR 0045). Each still has one place. The instance holds what a Git host cannot: whether automatic merging is used, the limit, branch naming and the toolchain image (ADR 0039).
- The list of hosts of a stage is in the instance, as a flag of the core, `kiln core --spec-hosts`, until a settings file exists. `kiln run` has no list, so a run started by hand is sealed as before (ADR 0059) (card 12, 2026-11-19).
- Kiln's own repo has a `CLAUDE.md` with its rules for every run and every session in it: the docs to read, Conventional Commits for commit messages and pull request titles, and that the repo is public. A run reads it from what is merged (rule 2). It is a protected place (ADR 0047) (card 11).

## ADR 0026: Kiln's repo lives in the developer's personal space on their company's GitHub; checks run on the developer's machine

- How the checks are defined (ADR 0045).
- How the runner is protected: isolated like a hosted runner, not sealed like a run (ADR 0046).
- Where tests run that need the container engine, which the runner does not have (ADR 0048).
- Kiln cannot act as an app on a personal repo on this Git host, so the fallback this decision names was taken: a public repo on github.com under the developer's personal account, with checks on the Git host's own runners (ADR 0049). GitHub as the first Git host, Kiln building no checker, and Kiln acting under its own identity all stand.
- Of this decision's cost rules, two stand: Kiln's repo causes no billable usage, and there are no hosted background jobs. The rules about the developer's own runner and its caches ended with that runner (ADR 0049). What may be stored on the Git host, and how CPU time per check run is recorded there (ADR 0031), is settled by the card that needs it.

## ADR 0030: Opus for spec and review, a model proposed per card for implement, Fable only as an advisor

- That a send-back happened is read from the number of send-backs on the card (ADR 0054). A session that is resumed after a send-back continues on the stronger model (ADR 0053).
- The core compares the model that a run's first event, `system/init`, names with the one it gave. A run on another model is stopped, saved and recorded with the model seen, and the command exits non-zero. Claude Code 2.1.326's output does not show the effort, so the effort recorded is the one given.
- The model of each stage is a flag of the core, `--model` for spec and `--implement-model` and `--review-model`, which default to `--model`, and the effort is fixed per stage: spec `high`, implement `medium`, review `high`. `kiln core --effort` is gone; `kiln run` by hand keeps its `--effort` (card 14, 2026-11-19).

## ADR 0031: Checks on Kiln's own repo stay fast by structure, not by a stopwatch

- The checks are one command in the repo, and these rules live in it (ADR 0045).
- The checks now run on the Git host's own runners and not on the developer's machine (ADR 0049). The rules stand, since those machines vary in speed as well.

## ADR 0032: Kiln keeps its data in one embedded database file

- The file is SQLite, and `sqlite3 <file> .dump` is the readable export of rule 2 (ADR 0057).
- The schema is version 2: besides cards and their entries, an `instance` table with one row, the instance's identity of eight hex digits, and a `runs` table with one row per run, with a run's ending empty while it is in flight. A run's row is kept for good, so that the measures can count it (ADR 0029, ADR 0030); the files of its record stay until its card is done or dropped (ADR 0053), and are removed by the card that first ends a card.
- The schema is version 3, with `specs`, one row per spec, written in the transaction that ends its run (card 11).
- The schema is version 5, after card 13's version 4 with the time a run started: `specs` holds every run's document, one row per run, with a column `kind`, `spec`, `report` or `review`; the rows that were there are specs (card 14, 2026-11-19).

## ADR 0034: Kiln is a core on the developer's machine, a proxy in a container, and one detached container per run

> Since ADR 0059 a run with a list of hosts has a way out, a fourth kind of process. The lines below still hold.

- The core is not started from a checkout but from a program built in a sealed container (ADR 0036, ADR 0037).
- The core does not depend on the directory it was started in: it makes its paths absolute at its start, runs git in the instance's copy of the repo and every other command, through the seal, in the data directory (card 16, 2026-11-20).
- `--page` is made absolute at start, like `--data` and `--repo` (2026-11-20).

## ADR 0035: The core keeps nothing in memory that matters; it reconciles the store, the container engine and the Git host

- A run's work is saved, then the run is recorded, and only then is its container removed (rule 3). The same command started again is a repeat of the run and not a retry: each step looks at what is there and goes on from it, so a repeat uses up nothing of ADR 0016.
- One core at a time holds a data directory: `kiln core` takes `flock` on `<dir>/lock` for the life of its process, and a second core, or `kiln card`, is refused. The kernel drops the lock with the process, also when a child such as `docker logs` lives on. The store's unique index refuses a second run in flight per card as well, since a lock alone does not guarantee one holder (card 8, 2026-11-18).
- The engine's things are known as an instance's by the label `kiln.instance` with the instance's eight hex digits, beside `kiln.run`, on a run's network and container (rule 2). Every tick sweeps first: what carries both labels and is not in flight in the store is removed (rule 4). Nothing without both labels is touched, and `kiln run` by hand sets no instance, so no core lists what it made (card 8).
- A run is written to the store between placing its container and starting it, so that a kill before leaves a container that never started, which the sweep removes, and a kill after leaves a run whose files are placed again into the container that is there, which is then started once.
- From the moment a run is in the store until its result is recorded, nothing in the core removes its container or network, and its container is never created again. A step that fails, an interrupt or a kill in between changes nothing, and the next tick goes on from what is there. Only the engine losing a container makes a run gone: one with no container and nothing recorded is failed (rule 4) (by the developer's decision of 2026-11-19, after card 8's second review).
- At a run's end the work is saved, then `result.json` is written, then the store, and only then are the container and network removed (rule 3) (card 8).
- A run's ID is `<instance>-<card>-<number of the card's runs + 1>`. It is the same on every tick until the run is in the store, and the engine allows one container per name, so a wait gets one run (card 8).
- A run with a list has a way out, the container `kiln-wayout-<id>`, with the labels `kiln.run=<id>`, `kiln.wayout=1` and, with an instance, `kiln.instance`. `RemoveRun` removes the save container, then the run's container, then the way out, then the network, each skipped when it is missing. `ListRuns` lists a way out of the instance as well, so that one left by a kill between two removals is swept. A way out without `kiln.run=<id>` is refused and left as it is (ADR 0059) (card 12, 2026-11-19).
- From the moment a run is in the store until its result is recorded, nothing in the core removes its way out or creates it again, as for its container and network. A stopped way out is started again before its run starts, and when a running run is carried again. A way out that is gone before its run has started leaves the run in flight, said on every tick; one that is gone while its run runs is said in the core's output, and the run goes on without the web (card 12, 2026-11-19).

## ADR 0037: Kiln is built in a sealed container, and the core contains no C code

- The database is pure Go: SQLite translated to Go, through `ncruces/go-sqlite3` (ADR 0057).

## ADR 0038: The container engine carries everything into and out of a run

> Since ADR 0059 a run with a list of hosts also reaches the hosts on its list. The lines below still hold.

- What is placed in a run, and how: the commit's files under `/work`, owned by the run's user, and the instruction at `/kiln/instruction.md`. The seal writes them as one archive into a container that has never started, and refuses absolute names, `..` and anything but files, directories and symlinks, so nothing else can be placed.
- The change is collected by saving the working tree in a throwaway container from the run image, with no network, the run's user and limits, and no mounts. The engine pipes the tree from the run's container into it, so nothing of it is unpacked on the developer's machine; what comes back is a tree and a pack of its new objects.
- A stage's documents and the run's sessions are kept as the archives the engine hands over, `documents.tar` and `session.tar` in the run's record, and are never unpacked on the developer's machine.
- A run's `result.json` holds `hosts`, its list of hosts, empty without one. The record of a run with a list holds `wayout.jsonl`, the way out's line per request: the target, the addresses, whether it was allowed or refused, the reason and the status. A run whose way out was gone when it was collected has none, and its `result.json` says `"wayout": "gone"` (ADR 0059) (card 12, 2026-11-19).

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
- A run's way out shares the run's network and is on the outward network `kiln-web` as well. It has forwarding off in both families, no capabilities, `no-new-privileges` and a read-only root. `kiln-web` is an ordinary bridge with no traffic between its containers (`enable_icc=false`) and the label `kiln.web`; one of the name without the label is refused, so that a way out is never put on a network Kiln did not make. It is never removed (ADR 0059) (card 12, 2026-11-19).

## ADR 0042: A card's current state and its history are written together

- A card carries two more facts beside its stage and what it waits for, so that no rule has to read the history (ADR 0054).
- The one place is `change` in `internal/store`: one transaction that checks, writes the card and writes its entry (ADR 0057).

## ADR 0043: A card stores its stage and what it waits for; a run is in flight or has ended in one of four ways

- "The developer's approval" covers confirming a proposed drop, since a spec run ends with one of two results (ADR 0011).
- A card carries two more facts beside the two lists: its number of send-backs and whether its run was retried (ADR 0054).
- `change` in `internal/store` is the one place of rule 4, and the 11 valid pairings are one table in its code (ADR 0057).
- A run's ending is read off its record: finished when its container exited with 0, the result is `success`, the model is the one asked for, and the work is saved or nothing changed; failed otherwise. Until the retry card a failed run moves its card to "needs you" (ADR 0016). Until the stage cards a card waits in spec for a run, which is told the card's text; a finished run moves it to wait for the developer's approval (card 8, 2026-11-18).
- A finished spec run moves its card to wait for approval only with a spec it could read and a run that had the web. Otherwise the card goes under "needs you" with the reason, and the run stays finished (card 11).
- A card whose review passed waits in review for the developer's approval, a twelfth pairing, until the core publishes; ready to merge, review waiting for the Git host, comes with publishing. A finished implement run moves its card to review only with a commit saved; one that changed nothing stops under "needs you". A finished review run's card goes by the review's first line (card 14, 2026-11-19).
- Until dropped is a mark on the card, a dropped card is in done, waiting for nothing, and the reason of its last entry, the developer's, begins with "dropped by the developer", followed by the note; the board tells it apart by that entry. No column is added. `AddRun` refuses a run of a card that does not wait for one, and the tick's hold moves a card only from the state it read, so that a drop between the tick's read and the run's start wins (card 16, 2026-11-20).

## ADR 0044: The UI sends requests; the core tells it about changes over one one-way event stream

- The stream also carries the entries of the instance, such as a pause (ADR 0055).
- Until the API comes, `kiln card --data <dir> <text>` stands in for it: it writes a card that waits in spec for a run. It takes the core's lock, so it never writes beside a running core (rule 5). The API card removes it in the pull request that adds the API (card 8, 2026-11-18).
- The core serves the API's first endpoints, and the screen, on a loopback address (`kiln core --listen`, `127.0.0.1:4747` by default), and refuses a request that does not come from its own page: a Host that is not the listen address gets 403, and a POST must be JSON, with an Origin, if it has one, of the page's own. Until the event stream comes, the page asks every 2 seconds. `kiln card` stays beside the screen for now, and the commands `kiln cards` and `kiln spec` of card 11's spec are not built (card 13, 2026-11-19).
- A core started with `--page` serves a page that is not built into its program. Rule 4 holds for the core as it runs by default. The flag is for work on the screen, and the file is trusted like the program: whatever it holds is "the core's own page" for rule 6, and may act through the API (2026-11-20).
- The board's JSON says the core's `build`, the first 16 hex digits of the SHA-256 of the embedded page, and the page carries the build it was served with in `data-build`. A page that sees another build on the board reloads itself, before it reads that answer further. It does not reload while a dialog is open, and it reloads once per build: a page reloaded for a build and still served another says so and stays. A core rebuilt with the same page reloads no page (2026-11-20).
- With `--page`, the build is the file's as it is at that moment: the page is served with it and the board says it, so a page left open reloads itself when the file changes. A file that cannot be read has no build, and a page left open stays as it is (2026-11-20).

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
- `cmd/wayout/` is a place of the seal, and its image is under `images/` (ADR 0059) (card 12, 2026-11-19).

## ADR 0048: Tests that need the container engine are run by hand, behind the developer's approval

- Creating a run's network and container, and starting the proxy from the credential command, are functions of `internal/seal`. The caller gives no network, mount, environment or limit. What a run can reach, read and obtain is asserted from inside a run's container in `internal/seal`'s test.

## ADR 0049: Kiln's repo is public on GitHub, under the developer's personal account

- The engine tests are started by `check/in-image`: always on the Git host's runner, and on the developer's machine only with `--engine`, so that a test built from a card's branch does not run there unasked (ADR 0048). They are compiled in the toolchain image with the network off and run on the machine that runs the check, since the image has no engine.

## ADR 0056: Kiln's stage instructions live in one protected place

- The instructions are embedded in the core's program by the package `instructions`. A run's instruction is written once, when it starts, and a run in flight keeps it (card 11).
- `instructions/implement.md` and `instructions/review.md` are embedded beside `instructions/spec.md`. A run is told its stage's instruction and then, each under its heading, `## The card`, `## The approved spec`, `## The last review` and `## The change`, as its stage has them (card 14, 2026-11-19).
- A part of what a run is told that a run wrote (`## The last spec`, `## The last report`, `## The last review` and `## The approved spec`) is told inside a code fence with the info string `markdown`, as the change is told inside one with `diff`. The fence is backticks, one longer than the longest run of backticks in the part, so nothing in the part ends it and only the tick's own headings are headings. The card and the developer's note are not fenced, and the store keeps every document as the run wrote it (2026-11-20).
