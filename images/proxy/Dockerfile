# The proxy of ADR 0017 and ADR 0034: built in the toolchain image, then only the
# program and the CA bundle are copied onto an empty base. Build it with
# the repo's root as the context:
#   docker build --file images/proxy/Dockerfile .
ARG TOOLCHAIN=kiln-toolchain
FROM ${TOOLCHAIN} AS build
WORKDIR /src
COPY go.mod ./
COPY cmd/proxy ./cmd/proxy
RUN CGO_ENABLED=0 go build -trimpath -o /proxy ./cmd/proxy

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /proxy /proxy
USER 65532:65532
ENTRYPOINT ["/proxy"]
