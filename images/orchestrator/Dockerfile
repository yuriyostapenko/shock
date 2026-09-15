# syntax=docker/dockerfile:1.7
# Orchestrator, spawn-runner hook and session controller in one image:
# `claude` runs the orchestrator, `shock` the controller, and
# /hooks/spawn-runner is a symlink to `shock` that dispatches on argv[0].
# The build stage bind-mounts the context incl. .git so -buildvcs stamps
# version, commit and time (`shock version`). Runtime: distroless base-debian13.

ARG GO_VERSION=1.27
ARG CLAUDE_CODE_VERSION=2.1.270

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-trixie AS build
WORKDIR /src
ARG TARGETOS TARGETARCH
RUN --mount=type=bind,target=/src,readonly \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -buildvcs=true -ldflags "-s -w" -o /out/shock ./cmd/shock

FROM debian:trixie-slim AS fetch
ARG CLAUDE_CODE_VERSION
ARG TARGETARCH
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*
RUN case "${TARGETARCH}" in amd64) arch=linux-x64 ;; arm64) arch=linux-arm64 ;; *) echo "unsupported TARGETARCH ${TARGETARCH}" >&2; exit 1 ;; esac \
 && mkdir -p /out/hooks \
 && curl --retry 5 --retry-delay 2 -fsSL \
      "https://downloads.claude.ai/claude-code-releases/${CLAUDE_CODE_VERSION:?set with --build-arg CLAUDE_CODE_VERSION}/${arch}/claude" \
      -o /out/claude && chmod 0755 /out/claude \
 && ln -s /usr/local/bin/shock /out/hooks/spawn-runner

FROM gcr.io/distroless/base-debian13:nonroot AS runtime
COPY --from=fetch --chmod=0755 /out/claude /usr/local/bin/claude
COPY --from=build --chmod=0755 /out/shock /usr/local/bin/shock
COPY --from=fetch /out/hooks /hooks
ENV HOME=/home/nonroot \
    DISABLE_AUTOUPDATER=1
ENTRYPOINT ["claude"]
CMD ["self-hosted-runner", "orchestrator", "--help"]
