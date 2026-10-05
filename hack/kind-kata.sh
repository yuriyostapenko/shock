#!/usr/bin/env bash
# Installs Kata Containers (runtime-rs with QEMU, kata-deploy's default shim)
# into every node of a kind cluster, registers the kata-qemu-runtime-rs handler
# with containerd and applies the `kata` RuntimeClass. Safe to rerun.
# The host must expose /dev/kvm, /dev/vhost-vsock and /dev/vhost-net before the
# cluster is created: a kind node only sees devices present at its creation.
# Usage: hack/kind-kata.sh <cluster> <kata-release>
set -euo pipefail
cluster="${1:?kind cluster name}"
version="${2:?Kata release, e.g. 4.2.0}"
kind="${KIND:-kind}"
root="$(cd "$(dirname "$0")/.." && pwd)"

# Kata publishes no checksum file; pinned from the downloaded release asset.
declare -A sha256=(
  [4.2.0/amd64]=b828904fa3f1e49ddd7dc799c72cb1503cd1e772d354c3987c8d4189b2a623a8
)

# The subset of the ~1 GB bundle the QEMU runtime-rs configuration uses.
members=(
  ./opt/kata/runtime-rs/bin/containerd-shim-kata-v2
  ./opt/kata/share/defaults/kata-containers/runtime-rs/configuration-qemu-runtime-rs.toml
  ./opt/kata/bin/qemu-system-x86_64
  './opt/kata/share/kata-qemu/*'
  ./opt/kata/libexec/virtiofsd
  ./opt/kata/share/kata-containers/vmlinux.container
  ./opt/kata/share/kata-containers/kata-containers.img
)

fetch() { # <arch> -> directory holding opt/kata
  local arch="$1" dir="${root}/bin/kata-${version}-$1"
  local sum="${sha256[${version}/${arch}]:-}"
  [ -n "$sum" ] || { echo "no pinned sha256 for Kata ${version}/${arch}" >&2; exit 1; }
  if [ ! -x "${dir}/opt/kata/runtime-rs/bin/containerd-shim-kata-v2" ]; then
    mkdir -p "$dir"
    local tarball="${dir}.tar.zst"
    curl --retry 5 --retry-delay 2 -fsSL \
      "https://github.com/kata-containers/kata-containers/releases/download/${version}/kata-static-${version}-${arch}.tar.zst" -o "$tarball"
    echo "${sum}  ${tarball}" | sha256sum --check --quiet
    tar --zstd -xf "$tarball" -C "$dir" --wildcards "${members[@]}"
    # The kernel and image are symlinks into versioned files; extract their targets too.
    local share=./opt/kata/share/kata-containers
    tar --zstd -xf "$tarball" -C "$dir" \
      "${share}/$(readlink "${dir}/${share}/vmlinux.container")" \
      "${share}/$(readlink "${dir}/${share}/kata-containers.img")"
    rm -f "$tarball"
  fi
  echo "$dir"
}

for node in $("$kind" get nodes --name "$cluster"); do
  for dev in /dev/kvm /dev/vhost-vsock /dev/vhost-net; do
    docker exec "$node" test -c "$dev" || {
      echo "${node} has no ${dev}: Kata needs KVM; on the host run 'modprobe kvm vhost_vsock vhost_net', then recreate the cluster" >&2
      exit 1
    }
  done
  case "$(docker exec "$node" uname -m)" in
    x86_64) arch=amd64 ;;
    *) echo "Kata e2e supports amd64 nodes only" >&2; exit 1 ;;
  esac
  dir="$(fetch "$arch")"
  docker exec "$node" rm -rf /opt/kata
  docker cp "${dir}/opt/kata" "${node}:/opt/kata"
  # Debug output (guest console, agent and runtime logs) lands in containerd's
  # journal in the node, where a failing e2e run is diagnosed.
  docker exec -i "$node" bash -c 'd=/opt/kata/share/defaults/kata-containers/runtime-rs/config.d && mkdir -p "$d" && cat > "$d/90-e2e-debug.toml"' <<'TOML'
[hypervisor.qemu]
enable_debug = true
[agent.kata]
enable_debug = true
[runtime]
enable_debug = true
TOML
  docker exec -i "$node" bash -s <<'EOF'
set -euo pipefail
# runtime-rs joins the Pod's systemd cgroup over the system D-Bus, which
# kind's node image does not ship.
if ! systemctl is-active -q dbus; then
  apt-get update -qq
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends dbus >/dev/null
  systemctl start dbus
fi
# QEMU backs guest RAM with a shared file on /dev/shm (virtio-fs needs it);
# Docker caps a container's /dev/shm at 64M, which faults any guest larger.
mount -o remount,size=50% /dev/shm
cfg=/etc/containerd/config.toml
if ! grep -q 'runtimes.kata-qemu-runtime-rs]' "$cfg"; then
  case "$(sed -n 's/^version *= *//p' "$cfg")" in
    2) plugin='io.containerd.grpc.v1.cri' ;;
    3) plugin='io.containerd.cri.v1.runtime' ;;
    *) echo "unknown containerd config version in $cfg" >&2; exit 1 ;;
  esac
  # Mirrors what kata-deploy writes for its qemu-runtime-rs shim.
  cat >> "$cfg" <<TOML

[plugins."${plugin}".containerd.runtimes.kata-qemu-runtime-rs]
  runtime_type = "io.containerd.kata-qemu-runtime-rs.v2"
  runtime_path = "/opt/kata/runtime-rs/bin/containerd-shim-kata-v2"
  privileged_without_host_devices = true
  pod_annotations = ["io.katacontainers.*"]
  [plugins."${plugin}".containerd.runtimes.kata-qemu-runtime-rs.options]
    ConfigPath = "/opt/kata/share/defaults/kata-containers/runtime-rs/configuration-qemu-runtime-rs.toml"
TOML
fi
systemctl restart containerd
for _ in $(seq 30); do crictl info >/dev/null 2>&1 && exit 0; sleep 1; done
echo "containerd did not come back" >&2; exit 1
EOF
  echo "Kata ${version} installed on ${node}"
done

# The VMM runs inside the Pod's cgroup (sandbox_cgroup_only); kata-deploy's
# overhead for this shim keeps it from being OOM-killed under small limits.
kubectl apply -f - <<'EOF'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: kata
handler: kata-qemu-runtime-rs
overhead:
  podFixed:
    memory: 320Mi
    cpu: 250m
EOF
