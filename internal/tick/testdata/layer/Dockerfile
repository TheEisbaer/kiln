# The tick test's stand-in for Kiln's layer (images/run/Dockerfile), for the
# case of a repo with its own toolchain: the same layer without the download
# of Claude Code, with the stand-in for it, testdata/claude/claude, which the
# test copies beside this file. The core builds it as Kiln's layer, with the
# directory as the context and the repo's toolchain image as TOOLCHAIN.
ARG TOOLCHAIN=kiln-toolchain
FROM ${TOOLCHAIN}

COPY claude /tmp/claude
RUN set -eu; \
    install --owner root --group root --mode 0755 /tmp/claude /usr/local/bin/claude; \
    rm /tmp/claude

RUN set -eu; \
    useradd --uid 1000 --user-group --create-home kiln; \
    test "$(id -g kiln)" = 1000; \
    mkdir --mode 0755 /kiln; \
    install --directory --owner kiln --group kiln --mode 0755 /work /kiln/out
USER kiln
WORKDIR /work
