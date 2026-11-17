# The seal test's upstream: built in the toolchain image onto an empty base. Build
# it with the repo's root as the context:
#   docker build --file internal/seal/testdata/upstream/Dockerfile .
ARG TOOLCHAIN=kiln-toolchain
FROM ${TOOLCHAIN} AS build
WORKDIR /src
COPY go.mod ./
COPY internal/seal/testdata/upstream ./internal/seal/testdata/upstream
RUN CGO_ENABLED=0 go build -trimpath -o /upstream ./internal/seal/testdata/upstream

FROM scratch
COPY --from=build /upstream /upstream
USER 65532:65532
ENTRYPOINT ["/upstream"]
