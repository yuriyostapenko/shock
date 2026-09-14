# syntax=docker/dockerfile:1.7
# One image serves the orchestrator, the spawn-runner hook and the session
# controller: `claude` for the orchestrator process, `shock` for the hook and
# controller, and the two-line /hooks/spawn-runner shim baked in.
#
# The runtime stage follows Anthropic's deploy-doc recipe: debian:bookworm-slim
# plus the native claude binary from the Claude Code release location
# (https://code.claude.com/docs/en/self-hosted-environments-deploy#build-the-runner-image).
# Verify the download against the release's signed manifest in your own
# pipeline if your policy requires it.

ARG GO_VERSION=1.27
ARG CLAUDE_CODE_VERSION=2.1.270

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG TARGETOS TARGETARCH VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/shock ./cmd/shock

FROM debian:bookworm-slim AS runtime
ARG CLAUDE_CODE_VERSION
ARG TARGETARCH
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*
RUN case "${TARGETARCH}" in amd64) arch=linux-x64 ;; arm64) arch=linux-arm64 ;; *) echo "unsupported TARGETARCH ${TARGETARCH}" >&2; exit 1 ;; esac \
 && curl -fsSL "https://downloads.claude.ai/claude-code-releases/${CLAUDE_CODE_VERSION:?set with --build-arg CLAUDE_CODE_VERSION}/${arch}/claude" \
      -o /usr/local/bin/claude && chmod 0755 /usr/local/bin/claude \
 && claude --version
COPY --from=build /out/shock /usr/local/bin/shock
RUN mkdir -p /hooks \
 && printf '#!/bin/sh\nexec /usr/local/bin/shock hook spawn-runner "$@"\n' > /hooks/spawn-runner \
 && chmod 0755 /hooks/spawn-runner \
 && useradd --uid 10001 --create-home --shell /usr/sbin/nologin shock
USER 10001
WORKDIR /home/shock
ENV HOME=/home/shock \
    DISABLE_AUTOUPDATER=1
ENTRYPOINT ["claude"]
CMD ["self-hosted-runner", "orchestrator", "--help"]
