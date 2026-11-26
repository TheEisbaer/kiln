# Dependencies

Every direct dependency has one entry (ADR 0051) with four parts: what it is
for, what else was looked at, why this one, and its licence. A replaced choice
stays, marked with a `Replaced` line that says by what and why.

The check command reads `go.mod` and the `## Go modules` section below: each
direct dependency needs a complete entry there, and an entry whose dependency is
gone must be marked as replaced. The `## Other` section is kept by hand and is
not read by the check command.

## Go modules

### `github.com/ncruces/go-sqlite3`

- **For:** The store of cards and their history (`internal/store`, ADR 0032): SQLite through `database/sql`.
- **Looked at:** `modernc.org/sqlite`, SQLite transpiled from C, which is the more proven of the two but brings ten modules, one that must be pinned exactly, and about twice the compile time; `go.etcd.io/bbolt`, a key-value store with no SQL, whose readable export would be our own code; one JSON file rewritten atomically, with no dependency but a rewrite of the whole history at every change.
- **Why this one:** It is SQLite, so the measures of ADR 0007 are queries and macOS's own `sqlite3` reads the file (`.dump`). It is SQLite 3.54.1 translated to Go, so the core has no C code (ADR 0037), and it brings three modules: `github.com/ncruces/go-sqlite3-wasm/v6` v6.7.35401, `github.com/ncruces/julianday` v1.0.0 and `golang.org/x/sys` v0.50.0. ADR 0057 records the choice.
- **Licence:** MIT. Of its modules, `go-sqlite3-wasm` (the translation) is MIT-0, `julianday` is MIT and `golang.org/x/sys` is BSD-3-Clause. SQLite itself is in the public domain.

## Other

### The Go image

- **For:** The toolchain image of this repo (`images/toolchain/`), in which the check runs on the Git host's runner and on the developer's machine. The image also carries the Go modules of `go.mod` and `go.sum`, downloaded when it is built, and has the module proxy turned off (ADR 0057).
- **Looked at:** Installing Go from its release archive onto a plain base image, which would make the image ours to maintain for no gain; the Alpine variant of the Go image, which has no advantage for a program without C code.
- **Why this one:** It is the official `golang` image of Docker Hub's library, maintained by the Docker library maintainers (`github.com/docker-library/golang`) with the Go project's own toolchain from go.dev inside; that toolchain builds the core (ADR 0037), and the image is pinned by digest so that a moved tag changes nothing.
- **Licence:** Go is BSD-3-Clause. The Debian packages in the image carry their own licences; none of them becomes part of Kiln's programs.

### jq

- **For:** Tests of shell commands that parse JSON with `jq`, run in the check and in the runs of this repo: the toolchain image (`images/toolchain/`) carries it as `/usr/local/bin/jq`, and so does the run image built on it.
- **Looked at:** Debian's `jq` package from the base image's distribution, which is 1.7.1 in trixie, follows the distribution rather than a pin of ours, and needs `apt-get` with the package index at build time; `gojq`, a Go implementation, which would be a Go module and a build of our own for a tool that only tests use.
- **Why this one:** It is jq's own release: one static binary per platform, with no library to install, and the release's `sha256sum.txt` gives each file's SHA-256. The Dockerfile pins the version and the SHA-256 for amd64 and arm64, so a changed file fails the build, as with Claude Code.
- **Licence:** MIT. The binary also contains parts under the permissive notices of its `COPYING`: David M. Gay's `dtoa.c` and `g_fmt.c`, `decNumber` under the ICU licence, and code from Heimdal (BSD-3-Clause). It is downloaded into the toolchain image when that is built, and never committed or shipped with Kiln's programs.

### The checkout action

- **For:** Fetching the pull request's code onto the runner in `.github/workflows/check.yml`.
- **Looked at:** Fetching the code with `git` by hand in the workflow, which avoids third-party code but leaves the handling of pull request refs and credentials to us.
- **Why this one:** It is the Git host's own action and the common way to do this; it is pinned by commit (`actions/checkout` v7.0.1, `3d3c42e5aac5ba805825da76410c181273ba90b1`) because a tag can move.
- **Licence:** MIT.

### The cache actions

- **For:** Restoring and saving the two caches in `.github/workflows/`, the toolchain image and Go's build cache: `actions/cache/restore` in both workflows, `actions/cache/save` in `cache.yml` only.
- **Looked at:** `docker/build-push-action` with BuildKit's `gha` backend, which needs a second builder or the containerd image store, the cache token in the build's step, and changes to every image build in the tests; calling the cache service by hand.
- **Why this one:** It is the Git host's own action, it has restore and save as separate actions, and it honours `cache-mode`. Pinned by commit (`actions/cache` v6.1.0, `55cc8345863c7cc4c66a329aec7e433d2d1c52a9`) because a tag can move.
- **Licence:** MIT.

### Claude Code

- **For:** The agent in every run (ADR 0002), installed by Kiln's layer of the run image (`images/run/`) at a pinned version.
- **Looked at:** The install script, which fetches whatever version is current and sets up background updates; the npm package, which needs Node.js in the image to install; the apt and dnf repositories, which only toolchain images of those distributions can use.
- **Why this one:** The release bucket serves one self-contained binary per platform and a `manifest.json` with each binary's SHA-256, signed with the release key. The Dockerfile pins the version and the SHA-256 per platform, so a changed file fails the build, on any toolchain image with `curl` and `sha256sum`.
- **Licence:** Proprietary: © Anthropic PBC, all rights reserved, use subject to Anthropic's Commercial Terms of Service. It is downloaded into the run image when that is built, and never committed or redistributed with Kiln.

### The git command

- **For:** Saving a run's working tree: the save's script in `internal/seal` runs it in the save container, from the toolchain image the run image is built on. The core runs it on the developer's machine to build what is placed in a run and to take in the save.
- **Looked at:** A Go module that implements Git, which brings many dependencies of its own; bindings to Git's C library, which ADR 0037 rules out.
- **Why this one:** It is Git itself, there on the developer's machine and in the toolchain image, adds no Go module, and its plumbing commands are a stable interface.
- **Licence:** GPL-2.0. It is not part of Kiln's programs; Kiln runs it and does not ship it.

### The docker command

- **For:** Driving the container engine from `internal/seal` and from `check/in-image`: networks, containers, images, and copying files into a run's container.
- **Looked at:** The engine's Go client library, a Go module with many dependencies of its own; the engine's HTTP API over its socket directly, which would be Kiln's own client to write and keep up.
- **Why this one:** It is there wherever the engine is, on the developer's machine and on the Git host's runner, adds no Go module, and is the interface every measurement in the ADRs was made with. Kiln does not pin it; the engine's stable API is what the seal relies on.
- **Licence:** Apache-2.0. It is not part of Kiln's programs; they run it.

### The az command with its azure-devops extension

- **For:** The shipped Azure DevOps commands, `scripts/azure-devops/publish` and `scripts/azure-devops/check-result`: listing, opening and updating pull requests, linking the work item, and reading a pull request's threads, policies and failed builds' logs. They run on the developer's machine, with the developer's own sign-in.
- **Looked at:** Calling the Azure DevOps REST API with `curl`, which would leave the sign-in and its renewal to Kiln's commands and a token in a file of their own.
- **Why this one:** It is Microsoft's own command line, which the developer already uses and signs in to, so the commands hold no token. Kiln does not pin it; the commands check that the extension is there and never install it.
- **Licence:** MIT. It is not part of Kiln's programs; the commands run it and Kiln does not ship it.

### The gitleaks command

- **For:** The scan of the shipped Azure DevOps publish command, before anything leaves the machine: the change, its commit message, the pull request's title and text, through `gitleaks stdin` (ADR 0050).
- **Looked at:** A list of patterns of Kiln's own, which would have to be kept up as gitleaks' rules are.
- **Why this one:** ADR 0050 chose it: it is the common secret scanner, it reads standard input, exits 1 on a hit, and takes the developer's own rules from `GITLEAKS_CONFIG`. Kiln does not pin it.
- **Licence:** MIT. It is not part of Kiln's programs; the command runs it and Kiln does not ship it.
