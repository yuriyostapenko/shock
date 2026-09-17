# SHOCK

**Self-Hosted Orchestrator for Claude on Kubernetes**

SHOCK is a Helm chart and a companion Go controller for on-demand Claude Code
self-hosted runners. Each session gets a persistent workspace volume that
survives suspension and is reused when the session resumes. Runner compute
scales to zero between messages.

## Getting started

Prerequisites: Kubernetes 1.35+, the [agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox)
controller, and a Claude self-hosted environment whose key is in a Secret
(key `environment-secret`) in the target namespace. Cilium and Prometheus
Operator are optional (see [Requirements](#requirements)).

```sh
helm install shock oci://ghcr.io/yuriyostapenko/charts/shock --version X.Y.Z -n claude-runners --set environment.existingSecret=claude-environment
```

The released chart pins the SHOCK image and the default runner image by digest;
no image values are needed. The chart [README](charts/shock/README.md) covers
every value, upgrades and operations.

### Custom runner image

Sessions run in the runner image, so this is where toolchains live. Start from
the default image and add what your repositories need as root, then switch back
to the runner user:

```dockerfile
FROM ghcr.io/yuriyostapenko/shock-runner:X.Y.Z
USER root
RUN apt-get update && apt-get install -y --no-install-recommends libpq-dev && rm -rf /var/lib/apt/lists/*
USER 1000
```

References: [Anthropic's runner image recipe](https://code.claude.com/docs/en/self-hosted-environments-deploy#build-the-runner-image)
and this repository's [`images/runner/Dockerfile`](images/runner/Dockerfile).
Whatever the base, the contract is `claude` 2.1.224 or later, `git` 2.32 or
later, and a non-root user whose home is the PVC mount (`runner.storage.mountPath`)
with `runner.baseDir` inside it; the chart README's [Images](charts/shock/README.md#images)
section has the details. Point the chart at your image:

```yaml
runner:
  image:
    repository: registry.example.com/team/claude-runner
    tag: "2026.09"
    digest: ""          # optional sha256:... pin
    pullPolicy: IfNotPresent
  imagePullSecrets:
    - name: registry-credentials
```

Existing sessions keep their current Pod; a new image reaches each session at
its next spawn, after it sleeps and wakes.

**Status: first implementation.** The chart, the `shock` binary (hook and
session controller) and the image build are in this repository. Releases are
cut from `vX.Y.Z` tags and publish the image, the chart as an OCI artifact and a
GitHub Release together; see [CONTRIBUTING.md](CONTRIBUTING.md#releasing). Validated
against the upstream agent-sandbox controller in kind, against a real API
server in envtest, and in a live run against a Claude self-hosted environment
(see [Validation](#validation)).

## Design

- Anthropic's orchestrator invokes a fast `spawn-runner` hook that only
  declares session intent: create or patch the session's Sandbox, publish an
  immutable work-order Secret, stamp `pending-spawn`, exit. At the
  active-session cap (default 2) it exits 1 and the control plane re-offers.
- The upstream [agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox)
  controller manages each session's Sandbox, Pod and PVC.
- SHOCK's session controller is the single sequencer for sleep and wake. Every
  decision requires current-generation condition truth, not `operatingMode` or
  condition presence alone, and every write is pinned to the UID and
  resourceVersion it was computed from.
- Immutable, Sandbox-owned per-order Secrets and those conditional writes keep
  concurrent hook replicas and controller passes from overwriting newer intent.

The [implementation spec](plans/shock-spec.md) defines the architecture,
lifecycle, security boundaries, configuration and acceptance criteria. The
chart's [README](charts/shock/README.md) is the install, upgrade and
operations document.

## Requirements

- Kubernetes `>= 1.35` (derived from the pinned agent-sandbox release).
- The agent-sandbox controller, tested range **v1.0.2**, installed from
  upstream. SHOCK does not install or own its CRDs.
- A Claude Code self-hosted environment and its environment key.
- Storage for per-session PVCs; the default uses `ReadWriteOncePod`.
- A runner image; the chart defaults to the released `shock-runner` image, and
  the chart README documents the contract for bringing your own.

Optional, recommended:

- Cilium, for the default `network.mode: cilium`: default-deny egress by host
  name with TLS SNI enforcement. Without it, `kubernetes` mode filters by port
  only and `none` renders no policy.
- Prometheus Operator, for the default `monitoring.enabled: true`: PodMonitor
  and alert rules. Set it to `false` on clusters without the CRDs.

## Repository

| Path | Purpose |
| --- | --- |
| `charts/shock/` | The Helm chart and its README |
| `cmd/shock/` | One binary: `shock hook spawn-runner`, `shock session-controller` |
| `internal/naming/` | Names, labels and annotations shared by hook and controller |
| `internal/hook/` | The spawn-runner hook |
| `internal/sessioncontroller/` | The controller-runtime session controller |
| `internal/patch/` | UID + resourceVersion pinned merge patches |
| `images/orchestrator/Dockerfile` | Distroless image: `claude`, `shock`, `/hooks/spawn-runner` symlink |
| `images/runner/Dockerfile` | Default runner image: `claude`, git, ssh, `mise` and `uv` for root-free tool installs |
| `test/chart/` | Typed decode of the rendered Sandbox template, forced-field and RBAC checks |
| `test/envtest/` | API-concurrency tests against a real kube-apiserver |
| `test/e2e/` | kind e2e including agent-sandbox conformance |
| `plans/shock-spec.md` | Implementation spec, acceptance criteria and verification record |

## Build and test

Toolchain: Go 1.27, Helm 4, kind, Docker. `golangci-lint` and `setup-envtest`
install with `go install` (see the Makefile for pinned versions).

```sh
make build            # bin/shock
make test             # unit tests (fake client over the real v1beta1 types)
make lint             # golangci-lint
make helm-lint chart-golden
make envtest          # downloads kube-apiserver/etcd for ENVTEST_K8S, runs test/envtest
make e2e-kind         # creates kind cluster, installs agent-sandbox, runs test/e2e
make image IMAGE=ghcr.io/you/shock:dev
bin/shock version   # git tag or pseudo-version, commit, commit time, Go version
```

Releases: push a `vX.Y.0` tag on a `main` commit; `.github/workflows/release.yaml`
publishes `ghcr.io/yuriyostapenko/shock:X.Y.Z` and
`ghcr.io/yuriyostapenko/shock-runner:X.Y.Z`, the chart at
`oci://ghcr.io/yuriyostapenko/charts/shock:X.Y.Z` with both images pinned by
digest, and the GitHub Release. Patch versions are cut by
`.github/workflows/release-claude.yaml`, which checks Anthropic's release
bucket daily and releases `vX.Y.(Z+1)` when a newer Claude Code is out and
`main` holds nothing else unreleased; every other release bumps the minor. See
[CONTRIBUTING.md](CONTRIBUTING.md#releasing).

Which Claude Code a version bundles is in the chart's
`shock.invalid/claude-code-version` annotation, in an image label of the same
name and in the release notes; `make bump-claude` moves the pin locally.

## Validation

Unit, chart golden, envtest and kind e2e suites run in CI on Kubernetes 1.35
and 1.37 against agent-sandbox v1.0.2. The full lifecycle was also exercised
live against a Claude self-hosted environment; the spec's section 13 holds the
verification record.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Keep changes small enough to review and
validate independently; lifecycle changes need envtest and kind, not only unit
tests.

## License

[Apache License 2.0](LICENSE).
