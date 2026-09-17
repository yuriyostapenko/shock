#!/usr/bin/env bash
# Asserts that an image's bundled claude binary reports the expected version and
# still documents every flag the chart renders (charts/shock/templates/
# _helpers.tpl for the runner, orchestrator-deployment.yaml for the orchestrator).
# Usage: hack/smoke-claude.sh <image-ref> [expected-version]
set -euo pipefail

image="${1:?usage: hack/smoke-claude.sh <image-ref> [expected-version]}"
want="${2:-}"

run() { docker run --rm --entrypoint claude "$image" "$@"; }

got="$(run --version)"
if [[ -n "$want" && "$got" != "$want "* ]]; then
  echo "${image} reports '${got}', expected ${want}" >&2
  exit 1
fi

check() {
  local help="$1" flag
  shift
  for flag in "$@"; do
    grep -q -- "  ${flag}" <<<"$help" || { echo "${image}: claude no longer documents ${flag}" >&2; exit 1; }
  done
}

check "$(run self-hosted-runner --help)" \
  --base-dir --capacity --environment-secret-file --exit-if-unused-min --health-port \
  --kill-session-after-min --push-outcome-on-release --release-idle-session-min \
  --use-anthropic-git-proxy
check "$(run self-hosted-runner orchestrator --help)" \
  --environment-secret-file --expected-spawn-seconds --health-port \
  --hook-concurrency --hook-timeout --hooks-dir

echo "${image}: ${got}"
