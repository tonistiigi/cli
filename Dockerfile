# syntax=docker/dockerfile:1.1.7-experimental
ARG GO_VERSION=1.13.15

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS golang
ENV  CGO_ENABLED=0

FROM golang AS esc
ARG ESC_VERSION=v0.2.0
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=tmpfs,target=/go/src/ \
    GO111MODULE=on go get github.com/mjibson/esc@${ESC_VERSION}

FROM golang AS gotestsum
ARG GOTESTSUM_VERSION=v0.4.0
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=tmpfs,target=/go/src/ \
    GO111MODULE=on go get gotest.tools/gotestsum@${GOTESTSUM_VERSION}

FROM golang AS vndr
ARG VNDR_VERSION=v0.1.1
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=tmpfs,target=/go/src/ \
    GO111MODULE=on go get github.com/LK4D4/vndr@${VNDR_VERSION}

FROM golang AS dev
RUN  apk add --no-cache \
    bash \
    build-base \
    ca-certificates \
    coreutils \
    curl \
    git

CMD bash
ENV DISABLE_WARN_OUTSIDE_CONTAINER=1
ENV PATH=$PATH:/go/src/github.com/docker/cli/build

COPY --from=esc       /go/bin/* /go/bin/
COPY --from=vndr      /go/bin/* /go/bin/
COPY --from=gotestsum /go/bin/* /go/bin/

WORKDIR /go/src/github.com/docker/cli
COPY . .

FROM golang AS build
WORKDIR /go/src/github.com/docker/cli
COPY --from=tonistiigi/xx:golang / /
ARG TARGETPLATFORM
# ENV CGO_ENABLED=0
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=target=. \
    go build -o /out/docker ./cmd/docker

FROM scratch AS binaries
COPY --from=build /out/docker /
