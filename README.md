# SHOCK

**Self-Hosted Orchestrator for Claude on Kubernetes**

SHOCK is a Helm chart and a companion Go controller for on-demand Claude Code
self-hosted runners. Each session gets a persistent workspace volume that
survives suspension and is reused when the session resumes. Runner compute
scales to zero between messages.

**Status: first implementation.** The chart, the `shock` binary (hook and
session controller) and the image build are in this repository. No published
container image or chart release exists yet; build from source. The design has
been validated against the upstream agent-sandbox controller in kind and
against a real API server in envtest, but not yet against a live Claude
self-hosted environment (see [Validation](#validation)).

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
- A runner image satisfying the contract in the chart README.

## Repository

| Path | Purpose |
| --- | --- |
| `charts/shock/` | The Helm chart and its README |
| `cmd/shock/` | One binary: `shock hook spawn-runner`, `shock session-controller` |
| `internal/naming/` | Names, labels and annotations shared by hook and controller |
| `internal/hook/` | The spawn-runner hook |
| `internal/sessioncontroller/` | The controller-runtime session controller |
| `internal/patch/` | UID + resourceVersion pinned merge patches |
| `images/orchestrator/Dockerfile` | Multi-stage image: `claude`, `shock`, `/hooks/spawn-runner` shim |
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
```

## Validation

Ran on 2026-09-14 against agent-sandbox v1.0.2 and Kubernetes 1.35 (kind
`kindest/node:v1.35.0`, envtest 1.35.0):

- `go vet`, `golangci-lint`, unit tests for naming, hook and session controller.
- Chart lint, render in every network mode with the CRD absent, and the golden
  test that strict-decodes the rendered template into the typed Sandbox.
- envtest: stale hook, Wake, Sleep and acknowledgement patches conflict rather
  than overwrite; GC delete preconditions lose to a concurrent spawn and win
  the reverse race with a retryable hook exit; immutable Secrets; recreated
  Sandboxes derive new Secret names; interleaved concurrent hooks.
- kind e2e (`test/e2e`): agent-sandbox conformance items 1 to 7 including the
  pinned reason strings; fresh session, sleep within budget, redelivery no-op,
  resume on the same PVC with the marker file present, crash then clean wake,
  no second pod during a bounce against a live session, two sessions for one
  account, anchor removal exits 2 before any write, GC cascade with zero
  orphaned Secrets, GC sparing a woken session, hostile `runner.podTemplate`
  forced fields on the real Pod, and `helm upgrade` leaving sleeping sessions
  untouched. The hook ran impersonating the orchestrator ServiceAccount, so its
  Role was exercised.

Not run: a manual session against a live Claude self-hosted environment with
the real orchestrator process and a real runner image. The hook's environment
variable contract and exit codes were verified against Anthropic's
documentation, not against a live orchestrator.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Keep changes small enough to review and
validate independently; lifecycle changes need envtest and kind, not only unit
tests.

## License

[Apache License 2.0](LICENSE).
