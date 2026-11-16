# The toolchain image of Kiln's own repo (ADR 0039): the official Go image,
# pinned by digest. Change the pin by a card and record it in
# docs/dependencies.md.
FROM golang:1.27.4@sha256:fe517acaa580cc7915457080f850c37e618b3c1e750f98b58fc6429b605147a1

# Never download another toolchain, and never stamp a build from Git: the check
# runs with the network off and on a mounted copy that Git may not trust.
ENV GOTOOLCHAIN=local \
    GOFLAGS=-buildvcs=false
