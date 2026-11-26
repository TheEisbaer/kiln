# The toolchain image of Kiln's own repo (ADR 0039): the official Go image,
# pinned by digest. Change the pin by a card and record it in
# docs/dependencies.md.
FROM golang:1.27.4@sha256:5f1b98fa02fe396ebce2a0227a0e08309e11792915b9dd2e1f71053d7e7e6666

# Never download another toolchain, and never stamp a build from Git: the check
# runs with the network off and on a mounted copy that Git may not trust.
ENV GOTOOLCHAIN=local \
    GOFLAGS=-buildvcs=false

# jq, for the tests of shell commands that parse JSON with it. The release's
# static binary, pinned by version and by the SHA-256 of each architecture's
# file, from that release's sha256sum.txt. A card that changes the pin takes
# both sums from there. It comes before the modules, so that a change of
# go.mod or go.sum does not download it again. Record the pin in
# docs/dependencies.md.
RUN set -eu; \
    arch=$(dpkg --print-architecture); \
    case "$arch" in \
        amd64) sum=b1c22172dd303f3be49e935aa56aa48a8b7a46e0bc838b4997d3bb451495870f ;; \
        arm64) sum=8b85c817833814ddca00a144c33705546355afccf0cf39b188f3cdb48b852309 ;; \
        *) echo "no jq pinned for the architecture '$arch'" >&2; exit 1 ;; \
    esac; \
    curl --fail --silent --show-error --location --output /tmp/jq \
        "https://github.com/jqlang/jq/releases/download/jq-1.8.2/jq-linux-$arch"; \
    echo "$sum  /tmp/jq" | sha256sum --check --strict -; \
    install --owner root --group root --mode 0755 /tmp/jq /usr/local/bin/jq; \
    rm /tmp/jq; \
    jq --version; \
    test "$(jq --version)" = jq-1.8.2

# The modules of go.mod and go.sum come with the image, so that the check finds
# them with the network off. The copies are removed, and the proxy is turned off
# afterwards: a module the image lacks fails with "module lookup disabled", not
# with a network error. The build context is these two files and this one.
COPY go.mod go.sum /tmp/modules/
RUN cd /tmp/modules && go mod download && rm -rf /tmp/modules
ENV GOPROXY=off
