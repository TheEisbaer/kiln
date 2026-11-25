# The toolchain image of Kiln's own repo (ADR 0039): the official Go image,
# pinned by digest. Change the pin by a card and record it in
# docs/dependencies.md.
FROM golang:1.27.4@sha256:5f1b98fa02fe396ebce2a0227a0e08309e11792915b9dd2e1f71053d7e7e6666

# Never download another toolchain, and never stamp a build from Git: the check
# runs with the network off and on a mounted copy that Git may not trust.
ENV GOTOOLCHAIN=local \
    GOFLAGS=-buildvcs=false

# The modules of go.mod and go.sum come with the image, so that the check finds
# them with the network off. The copies are removed, and the proxy is turned off
# afterwards: a module the image lacks fails with "module lookup disabled", not
# with a network error. The build context is these two files and this one.
COPY go.mod go.sum /tmp/modules/
RUN cd /tmp/modules && go mod download && rm -rf /tmp/modules
ENV GOPROXY=off
