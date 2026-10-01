#!/usr/bin/env bash
# Creates the Cilium lab cluster the secret-injection e2e needs (spec section 9).
# Separate from `make e2e-setup`, which keeps kindnet: TLS interception needs
# Cilium's L7 proxy and its policy-secret sync, neither of which kindnet has.
set -euo pipefail

CLUSTER="${KIND_CILIUM_CLUSTER:-shock-cilium}"
NODE_IMAGE="${KIND_NODE_IMAGE:-kindest/node:v1.35.8}"
CILIUM_VERSION="${CILIUM_VERSION:-1.20.2}"

if ! kind get clusters | grep -qx "${CLUSTER}"; then
  # disableDefaultCNI: Cilium replaces kindnet. kube-proxy stays; SHOCK's
  # policies need no kube-proxy replacement.
  kind create cluster --name "${CLUSTER}" --image "${NODE_IMAGE}" --wait 120s --config=- <<'KIND'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  disableDefaultCNI: true
KIND
fi

kubectl config use-context "kind-${CLUSTER}"

helm repo add cilium https://helm.cilium.io >/dev/null
helm repo update cilium >/dev/null

# tls.secretSync + readSecretsOnlyFromSecretsNamespace are the SDS defaults for
# new installs; set them explicitly so the lab matches what the README requires.
# hubble.redact keeps an injected header out of the L7 flow log.
helm upgrade --install cilium cilium/cilium \
  --version "${CILIUM_VERSION}" \
  --namespace kube-system \
  --set ipam.mode=kubernetes \
  --set operator.replicas=1 \
  --set tls.secretSync.enabled=true \
  --set tls.readSecretsOnlyFromSecretsNamespace=true \
  --set hubble.enabled=true \
  --set hubble.relay.enabled=false \
  --set hubble.redact.enabled=true \
  --set hubble.redact.http.userInfo=true \
  --set "hubble.redact.http.headers.deny={Authorization,Proxy-Authorization}" \
  --wait --timeout 10m

kubectl -n kube-system rollout status ds/cilium --timeout=300s
kubectl -n kube-system rollout status deploy/cilium-operator --timeout=300s

# Pods created before Cilium became the CNI have no Cilium endpoint.
kubectl -n kube-system rollout restart deploy/coredns
kubectl -n kube-system rollout status deploy/coredns --timeout=180s

echo "cilium lab ready: context kind-${CLUSTER}, cilium ${CILIUM_VERSION}"
