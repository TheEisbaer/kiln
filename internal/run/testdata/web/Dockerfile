# The run image of the run test's web case: the run image, with the fake web
# hosts' certificate authority in the system store, which curl and Claude Code
# trust. The test makes the authority when it starts the hosts, so the context
# is a directory of the test's own that holds only ca.crt:
#   docker build --build-arg RUN=<run image> --file internal/run/testdata/web/Dockerfile <dir>
ARG RUN=kiln-run
FROM ${RUN}
USER root
COPY ca.crt /usr/local/share/ca-certificates/kiln-test.crt
RUN update-ca-certificates
USER kiln
