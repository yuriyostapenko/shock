#!/usr/bin/env bash
# Pins images/*/Dockerfile and the chart's claude-code-version annotation to the
# Claude Code version Anthropic's release bucket reports for a channel (latest
# or stable), after checking that its manifest lists both Linux platforms.
# Prints "<old> <new>"; prints "<v> <v>" when already current or when the
# channel is behind the pin. "check" only asserts the three pins agree.
set -euo pipefail

channel="${1:-latest}"
base="https://downloads.claude.ai/claude-code-releases"
dockerfiles=(images/orchestrator/Dockerfile images/runner/Dockerfile)
chart=charts/shock/Chart.yaml
annotation="shock.invalid/claude-code-version"
cd "$(dirname "$0")/.."

pin() { sed -n 's/^ARG CLAUDE_CODE_VERSION=//p' "$1"; }
chart_pin() { sed -n "s|^  ${annotation}: \"\\(.*\\)\"\$|\\1|p" "$chart"; }

current="$(pin "${dockerfiles[0]}")"
[[ -n "$current" ]] || { echo "${dockerfiles[0]} has no ARG CLAUDE_CODE_VERSION" >&2; exit 1; }
for f in "${dockerfiles[@]}"; do
  [[ "$(pin "$f")" == "$current" ]] || { echo "$f pins $(pin "$f"), not ${current}" >&2; exit 1; }
done
[[ "$(chart_pin)" == "$current" ]] || { echo "${chart} annotates $(chart_pin), not ${current}" >&2; exit 1; }

if [[ "$channel" == check ]]; then
  echo "$current"
  exit 0
fi

new="$(curl --retry 5 --retry-delay 2 -fsSL "${base}/${channel}")"
[[ "$new" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "unexpected version from ${channel}: ${new}" >&2; exit 1; }
if [[ "$new" == "$current" ]] || [[ "$(printf '%s\n%s\n' "$current" "$new" | sort -V | tail -n1)" == "$current" ]]; then
  [[ "$new" == "$current" ]] || echo "${channel} is at ${new}, behind the pinned ${current}; not downgrading" >&2
  echo "$current $current"
  exit 0
fi

manifest="$(curl --retry 5 --retry-delay 2 -fsSL "${base}/${new}/manifest.json")"
for p in linux-x64 linux-arm64; do
  jq -e --arg p "$p" '.platforms[$p].checksum | strings' <<<"$manifest" >/dev/null \
    || { echo "manifest for ${new} lacks ${p}" >&2; exit 1; }
done

for f in "${dockerfiles[@]}"; do
  sed -i.bak "s/^ARG CLAUDE_CODE_VERSION=${current}\$/ARG CLAUDE_CODE_VERSION=${new}/" "$f" && rm -f "$f.bak"
  [[ "$(pin "$f")" == "$new" ]] || { echo "$f was not rewritten" >&2; exit 1; }
done
sed -i.bak "s|^  ${annotation}: \"${current}\"\$|  ${annotation}: \"${new}\"|" "$chart" && rm -f "$chart.bak"
[[ "$(chart_pin)" == "$new" ]] || { echo "${chart} was not rewritten" >&2; exit 1; }

echo "$current $new"
