# syntax=docker/dockerfile:1.7
# Default runner image, following Anthropic's deploy-doc recipe
# (https://code.claude.com/docs/en/self-hosted-environments-deploy#build-the-runner-image):
# debian:trixie-slim, git and openssh-client, the native claude binary verified
# against the release manifest, and the doc's system git configuration.
#
# The runner user (uid 1000, matching the chart's default fsGroup) owns its
# home, /home/runner, which the chart mounts the per-session PVC on, with
# repositories checked out under /home/runner/workspace. Everything a session
# installs or configures under ~ therefore survives sleep. Sessions install
# tooling without root through the preinstalled user-space managers: mise
# (runtimes and CLIs) and uv (Python and Python tools). apt is
# present for clusters that run the Pod in a user namespace
# (runner.hostUsers: false with runAsUser 0), where container root is an
# unprivileged host user. Build your own image FROM this one to add toolchains.

ARG CLAUDE_CODE_VERSION=2.1.270
ARG MISE_VERSION=v2026.9.9
ARG UV_VERSION=0.12.14

FROM debian:trixie-slim AS fetch
ARG CLAUDE_CODE_VERSION MISE_VERSION UV_VERSION TARGETARCH
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl jq \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /out
RUN case "${TARGETARCH}" in amd64) claude_arch=linux-x64; mise_arch=linux-x64; uv_arch=x86_64 ;; arm64) claude_arch=linux-arm64; mise_arch=linux-arm64; uv_arch=aarch64 ;; *) echo "unsupported TARGETARCH ${TARGETARCH}" >&2; exit 1 ;; esac \
 && base="https://downloads.claude.ai/claude-code-releases/${CLAUDE_CODE_VERSION:?set with --build-arg CLAUDE_CODE_VERSION}" \
 && curl --retry 5 --retry-delay 2 -fsSL "${base}/manifest.json" -o manifest.json \
 && curl --retry 5 --retry-delay 2 -fsSL "${base}/${claude_arch}/claude" -o claude \
 && echo "$(jq -r ".platforms[\"${claude_arch}\"].checksum" manifest.json)  claude" | sha256sum --check \
 && chmod 0755 claude \
 && curl --retry 5 --retry-delay 2 -fsSL "https://github.com/jdx/mise/releases/download/${MISE_VERSION}/mise-${MISE_VERSION}-${mise_arch}.tar.gz" -o mise.tar.gz \
 && curl --retry 5 --retry-delay 2 -fsSL "https://github.com/jdx/mise/releases/download/${MISE_VERSION}/SHASUMS256.txt" -o mise-sums.txt \
 && grep "/mise-${MISE_VERSION}-${mise_arch}.tar.gz$" mise-sums.txt | sed 's#  .*#  mise.tar.gz#' | sha256sum --check \
 && tar -xzf mise.tar.gz && install -m 0755 mise/bin/mise /out/mise-bin \
 && curl --retry 5 --retry-delay 2 -fsSL "https://github.com/astral-sh/uv/releases/download/${UV_VERSION}/uv-${uv_arch}-unknown-linux-gnu.tar.gz" -o uv.tar.gz \
 && curl --retry 5 --retry-delay 2 -fsSL "https://github.com/astral-sh/uv/releases/download/${UV_VERSION}/uv-${uv_arch}-unknown-linux-gnu.tar.gz.sha256" -o uv.sha256 \
 && echo "$(cut -d' ' -f1 uv.sha256)  uv.tar.gz" | sha256sum --check \
 && tar -xzf uv.tar.gz && install -m 0755 "uv-${uv_arch}-unknown-linux-gnu/uv" /out/uv-bin && install -m 0755 "uv-${uv_arch}-unknown-linux-gnu/uvx" /out/uvx-bin

FROM debian:trixie-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl git openssh-client jq unzip xz-utils bzip2 libicu76 procps \
 && rm -rf /var/lib/apt/lists/* \
 && git config --system user.name "Claude" \
 && git config --system user.email "noreply@anthropic.com" \
 && git config --system --add safe.directory '*' \
 && groupadd --gid 1000 runner \
 && useradd --uid 1000 --gid 1000 --create-home --shell /bin/bash runner \
 && mkdir -p /home/runner/workspace \
 && chown -R runner:runner /home/runner
COPY --from=fetch --chmod=0755 /out/claude /usr/local/bin/claude
COPY --from=fetch --chmod=0755 /out/mise-bin /usr/local/bin/mise
COPY --from=fetch --chmod=0755 /out/uv-bin /usr/local/bin/uv
COPY --from=fetch --chmod=0755 /out/uvx-bin /usr/local/bin/uvx
USER 1000
WORKDIR /home/runner
ENV HOME=/home/runner \
    LANG=C.UTF-8 \
    PATH=/home/runner/.local/share/mise/shims:/home/runner/.local/bin:/usr/local/bin:/usr/bin:/bin \
    DISABLE_AUTOUPDATER=1
ENTRYPOINT ["claude"]
CMD ["self-hosted-runner", "--help"]
