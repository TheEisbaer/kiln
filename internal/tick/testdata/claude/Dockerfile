# The tick test's stand-in for Claude Code: the test's run image with the
# script claude in place of the pinned Claude Code. Build it with this
# directory as the context and the run image as RUN:
#   docker build --build-arg RUN=<run image> internal/tick/testdata/claude
ARG RUN=kiln-run
FROM ${RUN}
COPY --chmod=0755 claude /usr/local/bin/claude
