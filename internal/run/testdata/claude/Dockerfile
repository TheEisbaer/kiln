# The run test's scripted upstream, which stands in for Claude: built in the
# toolchain image onto an empty base. Build it with the repo's root as the
# context:
#   docker build --file internal/run/testdata/claude/Dockerfile .
ARG TOOLCHAIN=kiln-toolchain
FROM ${TOOLCHAIN} AS build
WORKDIR /src
COPY go.mod ./
COPY internal/run/testdata/claude ./internal/run/testdata/claude
RUN CGO_ENABLED=0 go build -trimpath -o /claude ./internal/run/testdata/claude

FROM scratch
COPY --from=build /claude /claude
USER 65532:65532
ENTRYPOINT ["/claude"]
