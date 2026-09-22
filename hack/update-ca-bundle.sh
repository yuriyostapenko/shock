#!/usr/bin/env bash
# Refreshes charts/shock/files/upstream-ca-bundle.pem: the public roots Cilium
# verifies an intercepted host against (originatingTLS, spec section 9).
#
# The source is Mozilla's CA list as curl.se publishes it, verified against the
# digest curl.se publishes beside it. It is deliberately NOT the local trust
# store: a machine behind a TLS-inspecting proxy carries that proxy's CA, and an
# earlier version of this script shipped six of them into the chart.
#
# `check` re-verifies the committed file against its recorded digest offline,
# which is what CI runs.
set -euo pipefail

BUNDLE_URL="${BUNDLE_URL:-https://curl.se/ca/cacert.pem}"
out="charts/shock/files/upstream-ca-bundle.pem"
mode="${1:-fetch}"

# recorded_digest prints the sha256 the file header claims.
recorded_digest() {
  sed -n 's/^# sha256:[[:space:]]*//p' "$out" | head -1 | tr -d '[:space:]'
}

# body_digest prints the sha256 of everything after the chart's header, which is
# the upstream file byte for byte.
body_digest() {
  sed '1,/^# ---8<--- upstream file follows/d' "$out" | sha256sum | cut -d' ' -f1
}

if [ "$mode" = "check" ]; then
  [ -r "$out" ] || { echo "missing $out; run 'make ca-bundle'" >&2; exit 1; }
  want="$(recorded_digest)"
  got="$(body_digest)"
  if [ -z "$want" ]; then
    echo "$out records no sha256 in its header" >&2
    exit 1
  fi
  if [ "$want" != "$got" ]; then
    echo "$out does not match its recorded digest" >&2
    echo "  recorded: $want" >&2
    echo "  actual:   $got" >&2
    echo "The file was edited by hand or corrupted. Re-run 'make ca-bundle'." >&2
    exit 1
  fi
  echo "ok: $out matches its recorded digest ($want)"
  exit 0
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

curl --retry 5 --retry-delay 2 -fsSL "$BUNDLE_URL" -o "$tmp/cacert.pem"
curl --retry 5 --retry-delay 2 -fsSL "$BUNDLE_URL.sha256" -o "$tmp/cacert.pem.sha256"
# curl.se publishes "<digest>  cacert.pem"; check it in the directory holding both.
( cd "$tmp" && sha256sum --check cacert.pem.sha256 >/dev/null ) || {
  echo "digest check FAILED for $BUNDLE_URL: refusing to write $out" >&2
  exit 1
}

digest="$(cut -d' ' -f1 < "$tmp/cacert.pem.sha256")"
count="$(grep -c 'BEGIN CERTIFICATE' "$tmp/cacert.pem")"
{
  echo "# Public CA roots for Cilium's originatingTLS: what Cilium verifies the real"
  echo "# host against when it re-originates an intercepted connection (spec section 9)."
  echo "#"
  echo "# Source:  $BUNDLE_URL (Mozilla's CA list, as curl.se publishes it)"
  echo "# Fetched: $(date -u +%Y-%m-%d)"
  echo "# sha256:  $digest"
  echo "# Certificates: $count"
  echo "#"
  echo "# Verify this file yourself: the digest above is published at"
  echo "# $BUNDLE_URL.sha256 and covers everything below the marker."
  echo "# Refresh with 'make ca-bundle'; 'make ca-bundle-check' re-verifies it offline."
  echo "# ---8<--- upstream file follows"
  cat "$tmp/cacert.pem"
} > "$out"

echo "wrote $out ($count certificates, sha256 $digest)"
