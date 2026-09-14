# SHOCK

**Self-Hosted Orchestrator for Claude on Kubernetes**

SHOCK is a planned Helm chart and companion Go controller for on-demand Claude Code
self-hosted runners. Each session gets a persistent workspace volume that survives
suspension and is reused when the session resumes. Runner compute scales to zero
between active runs.

**Status: specification and repository scaffolding only.** There is no installable
chart, controller binary, or published container image yet.

## Design

- Anthropic's orchestrator invokes a fast spawn hook to declare session intent.
- The upstream agent-sandbox controller manages each session's Sandbox, Pod, and PVC.
- SHOCK's session controller coordinates suspension, wake-up, and idle-session cleanup.
- Immutable work-order Secrets and conditional Kubernetes mutations keep concurrent
  requests from overwriting newer session intent.

The [implementation spec](plans/shock-spec.md) defines the architecture, lifecycle,
security boundaries, configuration, and acceptance criteria. Read it before implementing.

## Planned requirements

- A supported Kubernetes cluster with the upstream
  [agent-sandbox controller](https://github.com/kubernetes-sigs/agent-sandbox) installed
  separately. SHOCK will not install or own its CRDs.
- Access to a Claude Code self-hosted environment.
- Persistent storage suitable for per-session PVCs; the default design uses
  `ReadWriteOncePod`.
- A runner image satisfying the contract in the spec.

Exact dependency versions and protocol assumptions must be verified before implementation.
Installation instructions will accompany the working chart.

## Repository

| Path | Purpose |
| --- | --- |
| `plans/shock-spec.md` | Implementation spec and acceptance criteria |
| `AGENTS.md` | Instructions for coding agents |
| `CONTRIBUTING.md` | Development and contribution guidance |

The planned source layout is documented in section 3 of the spec. Source directories,
build commands, and CI will be added with the first implementation rather than empty stubs.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Start with the spec's verification items;
keep changes small enough to review and validate independently.

## License

[Apache License 2.0](LICENSE).
