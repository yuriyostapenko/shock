#!/usr/bin/env bash
# Regenerates charts/shock/files/anthropic-trusted-domains.txt from the
# "Default allowed domains" section of Anthropic's cloud environments doc:
# the allow list behind the Trusted network level of Anthropic-hosted
# environments. One host per line, grouped as in the doc; wildcards keep the
# doc's `*.` spelling and are translated by the chart at render time.
set -euo pipefail
url="https://code.claude.com/docs/en/cloud-environments.md"
out="$(dirname "$0")/../charts/shock/files/anthropic-trusted-domains.txt"
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
curl --retry 5 --retry-delay 2 -fsSL "$url" -o "$tmp"
{
  echo "# Anthropic's Trusted-level default allowed domains."
  echo "# Source: ${url}#default-allowed-domains"
  echo "# Fetched: $(date -u +%Y-%m-%d) by hack/update-trusted-domains.sh; regenerate with: make trusted-domains"
  echo "# Enabled by network.anthropicTrustedDomains; a leading '*.' matches every subdomain."
  awk '
    /^## Default allowed domains/ { on = 1; next }
    on && /^## / { exit }
    on && /<Accordion title=/ { t = $0; sub(/.*title="/, "", t); sub(/".*/, "", t); print ""; print "# " t; next }
    on && /^ *\* / {
      h = $0
      sub(/^ *\* /, "", h)
      if (match(h, /^\[[^]]*\]/)) { h = substr(h, RSTART + 1, RLENGTH - 2) }   # [www.x.com](http://www.x.com) -> www.x.com
      sub(/ +\(.*\)$/, "", h)                                                  # trailing "(PHP Composer)"
      gsub(/\\/, "", h)                                                        # \*.gcr.io, raw\.githubusercontent.com
      sub(/ +$/, "", h)
      if (h != "") print h
    }
  ' "$tmp"
} > "$out"
n=$(grep -cvE '^(#|$)' "$out")
[ "$n" -gt 100 ] || { echo "only $n hosts parsed; doc layout changed?" >&2; exit 1; }
echo "wrote $out ($n hosts)"
