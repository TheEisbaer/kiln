# Dependencies

Every direct dependency has one entry (ADR 0051) with four parts: what it is
for, what else was looked at, why this one, and its licence. A replaced choice
stays, marked with a `Replaced` line that says by what and why.

The check command reads `go.mod` and the `## Go modules` section below: each
direct dependency needs a complete entry there, and an entry whose dependency is
gone must be marked as replaced. The `## Other` section is kept by hand and is
not read by the check command.

## Go modules

## Other

### The Go image

- **For:** The toolchain image of this repo (`images/toolchain/`), in which the check runs on the host's runner and on the developer's machine.
- **Looked at:** Installing Go from its release archive onto a plain base image, which would make the image ours to maintain for no gain; the Alpine variant of the Go image, which has no advantage for a program without C code.
- **Why this one:** It is the Go project's own official image, it carries the toolchain that builds the core (ADR 0037), and it is pinned by digest so that a moved tag changes nothing.
- **Licence:** Go is BSD-3-Clause. The Debian packages in the image carry their own licences; none of them becomes part of Kiln's programs.

### The checkout action

- **For:** Fetching the pull request's code onto the runner in `.github/workflows/check.yml`.
- **Looked at:** Fetching the code with `git` by hand in the workflow, which avoids third-party code but leaves the handling of pull request refs and credentials to us.
- **Why this one:** It is the Git host's own action and the common way to do this; it is pinned by commit (`actions/checkout` v7.0.1, `3d3c42e5aac5ba805825da76410c181273ba90b1`) because a tag can move.
- **Licence:** MIT.
