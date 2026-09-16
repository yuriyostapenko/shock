#!/usr/bin/env bash
# Pins images/*/Dockerfile to the Claude Code version Anthropic's release
# bucket reports for a channel (latest or stable), after checking that its
# manifest lists both Linux platforms. Prints "<old> <new>"; prints "<v> <v>"
# when already current or when the channel is behind the pin.
set -euo pipefail
channel="${1:-latest}"
base="https://downloads.claude.ai/claude-code-releases"
files=(images/orchestrator/Dockerfile images/runner/Dockerfile)
cd "$(dirname "$0")/.."

current="$(sed -n 's/^ARG CLAUDE_CODE_VERSION=//p' images/orchestrator/Dockerfile)"
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
for f in "${files[@]}"; do
  grep -q "^ARG CLAUDE_CODE_VERSION=${current}$" "$f" || { echo "$f does not pin ${current}" >&2; exit 1; }
  sed -i.bak "s/^ARG CLAUDE_CODE_VERSION=${current}$/ARG CLAUDE_CODE_VERSION=${new}/" "$f" && rm -f "$f.bak"
done
echo "$current $new"
