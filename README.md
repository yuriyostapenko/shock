# SHOCK

**Self-Hosted Orchestrator for Claude on Kubernetes**

SHOCK is a Helm chart and a companion Go controller for on-demand Claude Code
self-hosted runners. Each session gets a persistent workspace volume that
survives suspension and is reused when the session resumes. Runner compute
scales to zero between messages.

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
  immutable work-order Secret, stamp `pending-spawn`, exit.
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

Releases: push a `vX.Y.Z` tag on a `main` commit; `.github/workflows/release.yaml`
publishes `ghcr.io/yuriyostapenko/shock:X.Y.Z` and
`ghcr.io/yuriyostapenko/shock-runner:X.Y.Z`, the chart at
`oci://ghcr.io/yuriyostapenko/charts/shock:X.Y.Z` with both images pinned by
digest, and the GitHub Release.

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
