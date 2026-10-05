#!/usr/bin/env bash
# Installs gVisor (runsc, its sidecars and the containerd shim) into every node
# of a kind cluster, registers the runsc handler with containerd and applies
# the `gvisor` RuntimeClass. Safe to rerun on a reused cluster.
# Usage: hack/kind-gvisor.sh <cluster> <gvisor-release>
set -euo pipefail
cluster="${1:?kind cluster name}"
version="${2:?gVisor release, e.g. 20260928.0}"
kind="${KIND:-kind}"
root="$(cd "$(dirname "$0")/.." && pwd)"

# Pinned per release: update both when bumping GVISOR_VERSION.
declare -A sha512=(
  [20260928.0/x86_64]=c8d3a9fd4d4c4f5b8ff213caa4517356be128d18659ec4cde37828fe797f61a9725a602a846c81a8ed19c057a996515d31c081eba343ed4613a89951ba32ed59
  [20260928.0/aarch64]=926538a4f20056d44838f230297ecec9192db2562e2523a207295f706b76126725f2ff7b4e59d747147510c5714057eeec862a0f77d43bf625746592b5f51b00
)

fetch() { # <arch> -> extracted directory
  local arch="$1" dir="${root}/bin/gvisor-${version}-$1"
  local sum="${sha512[${version}/${arch}]:-}"
  [ -n "$sum" ] || { echo "no pinned sha512 for gVisor ${version}/${arch}" >&2; exit 1; }
  if [ ! -x "${dir}/runsc" ]; then
    mkdir -p "$dir"
    curl --retry 5 --retry-delay 2 -fsSL \
      "https://storage.googleapis.com/gvisor/releases/release/${version}/${arch}/gvisor.tar.bz2" -o "${dir}.tar.bz2"
    echo "${sum}  ${dir}.tar.bz2" | sha512sum --check --quiet
    tar -xjf "${dir}.tar.bz2" -C "$dir"
    rm -f "${dir}.tar.bz2"
  fi
  echo "$dir"
}

for node in $("$kind" get nodes --name "$cluster"); do
  dir="$(fetch "$(docker exec "$node" uname -m)")"
  # runsc finds its sidecars in gvisor-bin/ next to itself.
  docker exec "$node" rm -rf /usr/local/bin/gvisor-bin
  docker cp "${dir}/gvisor-bin" "${node}:/usr/local/bin/gvisor-bin"
  docker cp "${dir}/runsc" "${node}:/usr/local/bin/runsc"
  docker cp "${dir}/containerd-shim-runsc-v1" "${node}:/usr/local/bin/containerd-shim-runsc-v1"
  docker exec -i "$node" bash -s <<'EOF'
set -euo pipefail
cfg=/etc/containerd/config.toml
# kind's nodes cgroup through systemd; runsc must use the same driver.
printf '[runsc_config]\n  systemd-cgroup = "true"\n' > /etc/containerd/runsc.toml
if ! grep -q 'runtimes.runsc]' "$cfg"; then
  case "$(sed -n 's/^version *= *//p' "$cfg")" in
    2) plugin='io.containerd.grpc.v1.cri' ;;
    3) plugin='io.containerd.cri.v1.runtime' ;;
    *) echo "unknown containerd config version in $cfg" >&2; exit 1 ;;
  esac
  cat >> "$cfg" <<TOML

[plugins."${plugin}".containerd.runtimes.runsc]
  runtime_type = "io.containerd.runsc.v1"
  [plugins."${plugin}".containerd.runtimes.runsc.options]
    TypeUrl = "io.containerd.runsc.v1.options"
    ConfigPath = "/etc/containerd/runsc.toml"
TOML
fi
systemctl restart containerd
for _ in $(seq 30); do crictl info >/dev/null 2>&1 && exit 0; sleep 1; done
echo "containerd did not come back" >&2; exit 1
EOF
  echo "gVisor ${version} installed on ${node}"
done

kubectl apply -f - <<'EOF'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: gvisor
handler: runsc
EOF
