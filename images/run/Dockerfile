# Kiln's layer of a run image (ADR 0039): the pinned Claude Code and a user that
# is not root, on top of a toolchain image. Build it with this directory as the
# context:
#   docker build images/run
#
# Claude Code is pinned by version and by the SHA-256 of each platform's binary,
# from that release's manifest.json. A card that changes the pin reads Claude
# Code's changelog between the two versions, and updates the test data that
# follows the pin: the scripted upstream in internal/run/testdata/claude/.
ARG TOOLCHAIN=kiln-toolchain
FROM ${TOOLCHAIN}

# Set by BuildKit to the platform the image is built for.
ARG TARGETARCH
RUN set -eu; \
    case "${TARGETARCH:-}" in \
        arm64) platform=linux-arm64 sum=98d56f6ffe53469767a74864a38eaf359d62ef557a8de1b1ad524d61a66ec119 ;; \
        amd64) platform=linux-x64 sum=b6b3647d981130dc44fc670846ae97b6cde531ffb667cf9c4d17cfcafb38cb37 ;; \
        *) echo "no Claude Code pinned for the architecture '${TARGETARCH:-}'" >&2; exit 1 ;; \
    esac; \
    curl --fail --silent --show-error --location --output /tmp/claude \
        "https://downloads.claude.ai/claude-code-releases/2.1.326/$platform/claude"; \
    echo "$sum  /tmp/claude" | sha256sum --check --strict -; \
    install --owner root --group root --mode 0755 /tmp/claude /usr/local/bin/claude; \
    rm /tmp/claude

# Claude Code refuses bypassPermissions as root. The run's working tree is
# /work, and the documents of a stage go to /kiln/out (ADR 0038); both are the
# run's own. /kiln itself is root's: the seal places the instruction there.
RUN set -eu; \
    useradd --uid 1000 --user-group --create-home kiln; \
    test "$(id -g kiln)" = 1000; \
    mkdir --mode 0755 /kiln; \
    install --directory --owner kiln --group kiln --mode 0755 /work /kiln/out
USER kiln
WORKDIR /work
