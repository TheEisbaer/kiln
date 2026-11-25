# Kiln

Kiln takes a developer's ideas and turns them into changes in the repos that developer works in, with agents doing the work between idea and change. Each idea is a card that becomes one pull request in one repo: agents write its spec, implement it and review it, and the developer approves the spec once, before any code is written. Every developer runs an instance of their own, made of a core on their machine, a proxy in a container that carries the runs' requests to Claude, and one sealed container for each run.

Kiln is still being built; what it has to do first is the POC list in [docs/phases.md](docs/phases.md).

## Starting it

`scripts/kiln-up` does steps 1 to 4 and opens the screen; `scripts/kiln-down` stops the proxy. Run it from a terminal of your own: it refuses to run inside a Claude Code session. It builds each image, and the program, only when its files changed, makes the copy once and fetches its `main`, and prints neither the upstream nor the credential command. It needs `docker`, `git` and `shasum`, and `jq` when it reads `~/.claude/settings.json`. Its variables:

- `KILN_DATA`: the data directory, `~/kiln-live/data` by default. The program is `kiln` in it.
- `KILN_SOURCE`: the checkout that is built, `~/work/kiln` by default. From a checkout with uncommitted changes the program is built without a version.
- `KILN_REPO`: the instance's copy, `~/kiln-live/repo.git` by default.
- `KILN_ORIGIN`: what the copy is cloned from when it is missing, `https://github.com/TheEisbaer/kiln.git` by default.
- `KILN_MODEL`: `claude-opus-5-5` by default. `KILN_IMPLEMENT_MODEL`: none by default.
- `KILN_CLAUDE_UPSTREAM` and `KILN_CLAUDE_CREDENTIAL_COMMAND`: else `.env.ANTHROPIC_BASE_URL` and `.apiKeyHelper` of `~/.claude/settings.json`.
- `KILN_PUBLISH_COMMAND` and `KILN_CHECK_RESULT_COMMAND`: the commands of `--publish-command` and `--check-result-command`, none by default.

The steps below are what the script does, and how to do it by hand. Kiln needs a container engine with the `docker` command, and Git. The commands run from the repo's root.

1. Build the images. The toolchain image comes first, since the others are built on it. Building the run image downloads the pinned Claude Code. The way out is needed only with `--spec-hosts`.

   ```sh
   tar -cf - go.mod go.sum images/toolchain/Dockerfile |
       docker build --tag kiln-toolchain --file images/toolchain/Dockerfile -
   docker build --tag kiln-proxy --file images/proxy/Dockerfile .
   docker build --tag kiln-wayout --file images/wayout/Dockerfile .
   docker build --tag kiln-run images/run
   ```

2. Build the program `kiln` from `cmd/core`, in the toolchain image with the network off. The lines below build for macOS on arm64; for Linux on x86, use `GOOS=linux` and `GOARCH=amd64`.

   ```sh
   docker run --rm --network none --volume "$PWD:/src:ro" --workdir /src \
       --env GOOS=darwin --env GOARCH=arm64 --env CGO_ENABLED=0 \
       kiln-toolchain sh -c 'go build -trimpath -o /tmp/kiln ./cmd/core && cat /tmp/kiln' >kiln.new &&
       chmod +x kiln.new && mv kiln.new kiln || { rm -f kiln.new; false; }
   ```

   Build from the head of `main`: a program older than the store refuses to open it, and the core then does not start. `kiln` with no arguments prints its commands and their flags.

3. Make the instance's copy of the target repo, a bare clone. Runs start from its `HEAD`, which must be `main`. While it runs, the core fetches `main` from the copy's `origin`, at most once in 30 seconds, before it starts a run or publishes a card, and under `kiln launch` also while it has nothing else to do, so that an update is seen. The fetch asks nothing: ssh runs in batch mode with no terminal, so a key with a passphrase needs an agent. A fetch that fails is printed, and the `main` that is there is used.

   ```sh
   git clone --bare <target repo> <bare repo>
   ```

4. Start the instance with the launcher, `kiln launch`, and the flags of the core. The core reads two variables, and does not start without them. `KILN_CLAUDE_UPSTREAM` is the upstream, Claude's API or a service in front of it, as an http or https URL. `KILN_CLAUDE_CREDENTIAL_COMMAND` is a command that prints a credential, the same contract as Claude Code's key helper.

   ```sh
   KILN_CLAUDE_UPSTREAM=<url> KILN_CLAUDE_CREDENTIAL_COMMAND='<command>' \
       ./kiln launch --data <dir> --repo <bare repo> --model <model id>
   ```

   The launcher starts `kiln core` with those flags and the same environment. Every core first compares the store, the container engine and the Git host, with the screen served, and acts only after that; until then the screen refuses moves. The launcher starts the core again after a crash, after 1 second and then up to 60 seconds. A core that ends before that comparison, or ends with 0, as on Ctrl-C, ends the launcher. `kiln core` is the command the launcher starts, and it can also run alone with the same flags.

   Under `kiln launch`, once `main` of Kiln's own repo has moved past the version the running core was built from, the core updates itself. It first builds `kiln-toolchain` from `main`'s head under the tag of its first 12 hex digits, `kiln-toolchain:<tag>`, from the engine's cache when nothing changed, and then the program of that head in it, sealed, into `<dir>/programs/kiln-<commit>`. Then it stops acting, copies the store beside its own version as `<dir>/programs/kiln-<version>.db`, or `kiln-by-hand.db` for a program built by hand, and asks the launcher to replace it. The runs in flight go on in their containers. After a failed update the previous program puts that copy back before it opens the store, says why, and does not update to the same commit again until `main` moves on. It also turns automatic merging off: a review that passes then waits for you under approval instead of being published. "Turn automatic merging on" on the screen ends this, and with `main` still at that commit it tries the update again. When the images' files changed (`images/`, `cmd/proxy/`, `cmd/wayout/`, `go.mod`, `go.sum`), or always for a core built by hand, which cannot tell, it builds `kiln-proxy`, `kiln-wayout` and `kiln-run` too, after the toolchain and before the program, each under `<name>:<tag>` as in step 1, the proxy and the way out with no network. None is tagged under its plain name: the new core does that after it has reported healthy, before it keeps the proxy, so after a failed update the previous core still finds its own images. The images of the version before stay in the engine untagged, and the update removes the 12-digit tags of versions more than one step back. Step 2 is the first install only. A core built by hand updates once one of its cards has merged since it started. A core without a launcher never updates, and says so once. If the launcher itself was killed while it put a copy back, the copy can be put back by hand, with Kiln stopped: `cp <dir>/programs/kiln-<version>.db <dir>/store.db && rm -f <dir>/store.db-journal`.

   The core keeps the proxy, the container `kiln-proxy` on the network `kiln`: one that runs with the image `kiln-proxy` as it is now and the same upstream is kept, so that the runs in flight keep it, and any other is replaced. The proxy starts without a credential. The core runs the command with `sh -c` in the data directory, with no standard input and for at most 30 seconds, and gives what it prints to the proxy inside the proxy's container, never through a port: at its start, before any run, then five minutes after each success, and at once when a run it follows shows that the upstream refused the credential. When the command fails, runs out of time or prints nothing, the core says so once with the command's standard error, the screen says "Paused" at the top with the reason, and no run starts until the command succeeds again, which is tried every 30 seconds; the runs in flight go on. The command's standard output is never printed or kept.

   A run that fails is run once more by itself; a second failure in a row in the same stage puts the card under "needs you". A run whose container never starts, or stays paused, restarting, being removed or dead, for 5 minutes, and one whose instruction or `documents.tar` was removed from the data directory, is ended as failed and goes the same way. A run the upstream refused the credential to is run again once the pause ends, and does not count.

   `--data` is the instance's data directory, which is made if it is missing. `--model` is the exact model identifier for spec runs. It also applies to implement and review runs, unless `--implement-model` or `--review-model` names another. An implement run uses the model its approved spec names on its line `Model for implement:`, if it is one of the three the spec instruction allows; otherwise `--implement-model`, or `--model` without it. After a send-back an implement run uses `claude-opus-5-5`, whatever its spec names. `--spec-hosts` names the hosts a spec run may reach. It is none by default, and `*` stands for every public host. `--runs` is the most runs in flight at once, 3 by default and from 1 to 8. Ctrl-C ends the core. The runs it was following go on in their containers.

   `--publish-command` is the developer's own command that publishes a card, run with `sh -c` in the data directory; empty, the default, no card is published, and a card whose review passed waits on the screen. Kiln holds no key and no token for the Git host: the command does, and lives outside the repo. When a card's review passes, the core publishes it by itself: it puts the change onto the present `main` as one commit and runs the command, for at most five minutes, with no standard input and with these variables:

   - `KILN_CARD`: the card's ID.
   - `KILN_REPO`: the instance's copy of the repo.
   - `KILN_COMMIT`: the commit to publish, also kept under `refs/kiln/publish/card-<id>`.
   - `KILN_BRANCH`: the branch to push it to, `card/<id>-<name>`.
   - `KILN_TITLE`: the pull request's title.
   - `KILN_TEXT_FILE`: a file with the pull request's text.
   - `KILN_WORKFLOW_YES`: `1` if the change touches a file under `.github/workflows/` and the developer said yes to it on the screen; unset otherwise.

   Exit status 0 means published, and the last line of standard output that starts with `https://` is the pull request's address. Any other status puts the card under "needs you" with the command's standard error, and the developer can publish again; the command may be run again for the same card and commit, and copes with a branch or a pull request that exists. A change that touches a file under `.github/workflows/` waits on the screen until the developer presses "Publish" and says yes to its workflow files. Once the change is in `main` after a fetch, the card is done as merged.

   `--check-result-command` is the developer's own command that reads the check of a pull request on the Git host, run with `sh -c` in the data directory; empty, the default, no check is read. For every card at the Git host with a pull request the core runs it at once, and then at most every two minutes, never twice at once for one card, for at most one minute with its process group, with no standard input, and with the core's environment without any name that begins with `KILN_`, plus one: `KILN_COMMIT`, the published commit. Nothing else is told. Exit status 0 with the last line `pending`, `passed` or `failed` is a result; no check yet for the commit is `pending`. `pending` and `passed` change nothing. `failed` sends the card back to implement by the rule, as a send-back: what the command printed before that line is the check's output, of which the end is kept, at most 16 KiB from a line start. It goes into the card's history and is told to the implement run, which starts from the card's branch; review and publishing then run as before. A card that was sent back twice already waits in implement for you instead. A failed check for a commit that is no longer the published one changes nothing, and one for a change that is in `main` already closes the card as merged. Anything else, another last line, another status or a command that runs too long, changes nothing and is said once on the core's error output, with at most 2 KiB of the command's standard error, which goes nowhere else. The command holds what reading the check needs and lives outside the repo: owner and repo are fixed in it, `KILN_COMMIT` is 40 lowercase hex digits and is only ever quoted, and standard output carries only the check's output and the result, never a token. On GitHub, the check runs of a commit are listed with a GitHub App token, not a fine-grained personal token. The runs of `check.yml` by `head_sha`, and their job logs, can be read with a fine-grained token that has only "Actions: read" on the one repo.

5. Open the screen at <http://127.0.0.1:4747/>. The core prints the address when it starts, and `--listen` serves the screen on another loopback address. Cards are written on the screen, or with `./kiln card --data <dir> <text>`. A card can name earlier cards as predecessors; it is queued until they are done, and then its spec run starts by itself. A card can also name one merged card that it fixes; the screen counts the merged cards that needed a follow-up fix. `./kiln run` carries out one run by hand, through the proxy that a core keeps and gives the credential. `--page <file>` reads the page from that file on every request instead of the embedded one, so that a change to it shows after a refresh; it is for work on the screen, and the file is trusted like the program itself. A page left open while the core is restarted with another page reloads itself once; with `--page` it reloads when the file changes.

## Where the decisions are

- [CONTEXT.md](CONTEXT.md): Kiln's words, and the words to avoid.
- [docs/adr/](docs/adr/): the decisions, one file each.
- [docs/settled.md](docs/settled.md): what cards have settled under each decision.
- [docs/open-points.md](docs/open-points.md): what the decisions do not settle yet, listed by the card that has to.
- [docs/phases.md](docs/phases.md): the POC and MVP lists.
- [docs/dependencies.md](docs/dependencies.md): every direct dependency, and why it was chosen.
- [CLAUDE.md](CLAUDE.md): the rules for all work in this repo.
