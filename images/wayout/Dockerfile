# A run's way out (ADR 0059): built in the toolchain image, then only the
# program is copied onto an empty base. It holds no credential and needs no
# certificate: it never looks inside a connection. Build it with the repo's
# root as the context:
#   docker build --file images/wayout/Dockerfile .
ARG TOOLCHAIN=kiln-toolchain
FROM ${TOOLCHAIN} AS build
WORKDIR /src
COPY go.mod ./
COPY cmd/wayout ./cmd/wayout
RUN CGO_ENABLED=0 go build -trimpath -o /wayout ./cmd/wayout

FROM scratch
COPY --from=build /wayout /wayout
USER 65532:65532
ENTRYPOINT ["/wayout"]
