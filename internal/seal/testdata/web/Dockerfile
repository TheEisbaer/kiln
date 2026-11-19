# The fake web host of the seal's test and the run's test: built in the
# toolchain image onto an empty base. Build it with the repo's root as the
# context:
#   docker build --file internal/seal/testdata/web/Dockerfile .
ARG TOOLCHAIN=kiln-toolchain
FROM ${TOOLCHAIN} AS build
WORKDIR /src
COPY go.mod ./
COPY internal/seal/testdata/web ./internal/seal/testdata/web
RUN CGO_ENABLED=0 go build -trimpath -o /web ./internal/seal/testdata/web

FROM scratch
COPY --from=build /web /web
USER 65532:65532
ENTRYPOINT ["/web"]
