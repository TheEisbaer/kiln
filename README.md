# Kiln

Kiln takes a developer's ideas and turns them into changes in the repos that developer works in, with agents doing the work between idea and change. Each idea is a card that becomes one pull request in one repo: agents write its spec, implement it and review it, and the developer approves the spec once, before any code is written. Every developer runs an instance of their own, made of a core on their machine, a proxy in a container that carries the runs' requests to Claude, and one sealed container for each run.

Kiln is still being built; what it has to do first is the POC list in [docs/phases.md](docs/phases.md).

## Starting it

Kiln needs a container engine with the `docker` command, and Git. The commands run from the repo's root.

1. Build the images. The toolchain image comes first, since the others are built on it. Building the run image downloads the pinned Claude Code. The way out is needed only with `--spec-hosts`.

   ```sh
   docker build --tag kiln-toolchain --file images/toolchain/Dockerfile .
   docker build --tag kiln-proxy --file images/proxy/Dockerfile .
   docker build --tag kiln-wayout --file images/wayout/Dockerfile .
   docker build --tag kiln-run images/run
   ```

2. Build the program `kiln` from `cmd/core`, in the toolchain image with the network off. The lines below build for macOS on arm64; for Linux on x86, use `GOOS=linux` and `GOARCH=amd64`.

   ```sh
   docker run --rm --network none --volume "$PWD:/src:ro" --workdir /src \
       --env GOOS=darwin --env GOARCH=arm64 --env CGO_ENABLED=0 \
       kiln-toolchain sh -c 'go build -trimpath -o /tmp/kiln ./cmd/core && cat /tmp/kiln' >kiln
   chmod +x kiln
   ```

   `kiln` with no arguments prints its commands and their flags.

3. Start the proxy. It reads two variables. `KILN_CLAUDE_UPSTREAM` is the upstream, Claude's API or a service in front of it, as an http or https URL. `KILN_CLAUDE_CREDENTIAL_COMMAND` is a command that prints a credential, the same contract as Claude Code's key helper.

   ```sh
   KILN_CLAUDE_UPSTREAM=<url> KILN_CLAUDE_CREDENTIAL_COMMAND='<command>' ./kiln proxy
   ```

   It starts the container `kiln-proxy` with the credential the command printed, and returns. To give the proxy a fresh credential, run it again: it replaces the proxy. What that means for a run in flight is under "The push card" in [docs/open-points.md](docs/open-points.md).

4. Make the instance's copy of the target repo, a bare clone. Runs start from its `HEAD`, which must be `main`. Kiln does not update the copy from the Git host yet.

   ```sh
   git clone --bare <target repo> <bare repo>
   ```

5. Start the core.

   ```sh
   ./kiln core --data <dir> --repo <bare repo> --model <model id>
   ```

   `--data` is the instance's data directory, which is made if it is missing. `--model` is the exact model identifier for spec runs. It also applies to implement and review runs, unless `--implement-model` or `--review-model` names another. `--spec-hosts` names the hosts a spec run may reach. It is none by default, and `*` stands for every public host. Ctrl-C ends the core. A run it was following goes on in its container.

6. Open the screen at <http://127.0.0.1:4747/>. The core prints the address when it starts, and `--listen` serves the screen on another loopback address. Cards are written on the screen, or with `./kiln card --data <dir> <text>`.

## Where the decisions are

- [CONTEXT.md](CONTEXT.md): Kiln's words, and the words to avoid.
- [docs/adr/](docs/adr/): the decisions, one file each.
- [docs/settled.md](docs/settled.md): what cards have settled under each decision.
- [docs/open-points.md](docs/open-points.md): what the decisions do not settle yet, listed by the card that has to.
- [docs/phases.md](docs/phases.md): the POC and MVP lists.
- [docs/dependencies.md](docs/dependencies.md): every direct dependency, and why it was chosen.
- [CLAUDE.md](CLAUDE.md): the rules for all work in this repo.
