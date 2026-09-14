# syntax=docker/dockerfile:1.7
# One image serves the orchestrator, the spawn-runner hook and the session
# controller: `claude` for the orchestrator process, `shock` for the hook and
# controller, and /hooks/spawn-runner as a symlink to `shock`, which runs the
# hook when invoked under that name.
#
# The runtime is gcr.io/distroless/base-debian13:nonroot: glibc, OpenSSL, CA
# certificates and tzdata, user 65532, /home/nonroot and /tmp writable. The
# native claude binary comes from the Claude Code release location
# (https://code.claude.com/docs/en/self-hosted-environments-deploy#build-the-runner-image).

ARG GO_VERSION=1.27
ARG CLAUDE_CODE_VERSION=2.1.270

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG TARGETOS TARGETARCH VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/shock ./cmd/shock

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
