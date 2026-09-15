# shock

Self-Hosted Orchestrator for Claude on Kubernetes. One chart installs:

- the Anthropic orchestrator (`claude self-hosted-runner orchestrator`) with the
  `spawn-runner` hook baked into its image,
- the SHOCK session controller, which sequences sleep, wake, spawn observation,
  idle garbage collection and stranded-session alarms,
- the Helm-rendered Sandbox template the hook stamps per session,
- RBAC, network policy and monitoring for all of the above.

Every Claude session gets its own `Sandbox` and a persistent workspace PVC that
survives sleep. Runner compute scales to zero between messages.

## Prerequisites

| Requirement | Notes |
| --- | --- |
| Kubernetes `>= 1.35` | Derived from agent-sandbox v1.0.2's `k8s.io/*` v0.37 pin minus two minors. Older clusters may work but are untested and unsupported. |
| [agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox) controller, tested range **v1.0.2** | Install from the upstream release manifests. This chart never renders its CRDs or controller. Until the CRD is served the session controller stays alive but not ready and the hook exits 1 (retryable). |
| A Claude self-hosted environment | Create it on the Cloud environments admin page and store the environment key in a Secret (key `environment-secret`). |
| Persistent storage for per-session PVCs | Default access mode `ReadWriteOncePod` (needs a CSI driver). Immutable per session after creation. |
| Cilium (default `network.mode: cilium`) | Or set `network.mode: kubernetes` (no FQDN filtering) or `none`. |
| Prometheus Operator (default `monitoring.enabled: true`) | PodMonitor and PrometheusRule CRDs. Set `monitoring.enabled: false` otherwise. |
| The SHOCK image and a runner image | Both published by the release; see below. |

## Install

Releases publish the chart as an OCI artifact at
`oci://ghcr.io/yuriyostapenko/charts/shock` and the image at
`ghcr.io/yuriyostapenko/shock`, both tagged `X.Y.Z` from the git tag `vX.Y.Z`.
The released chart's `appVersion` is that same `X.Y.Z` and its default values
pin the released image by digest, so the orchestrator image needs no values.

```sh
kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v1.0.2/sandbox.yaml
kubectl create namespace claude-runners
(umask 077 && cat > ./environment-secret)   # paste the environment key, Enter, Ctrl-D
kubectl -n claude-runners create secret generic claude-environment --from-file=environment-secret=./environment-secret && rm ./environment-secret

helm install shock oci://ghcr.io/yuriyostapenko/charts/shock --version X.Y.Z -n claude-runners \
  --set environment.existingSecret=claude-environment
```

From a checkout, `Chart.yaml` carries `0.0.0-dev`; pass `orchestrator.image.tag`
and `runner.image.tag` (and optionally the digests) yourself.

Both artifacts are signed keyless with cosign and carry GitHub build provenance:

```sh
cosign verify ghcr.io/yuriyostapenko/shock:X.Y.Z \
  --certificate-identity-regexp '^https://github.com/yuriyostapenko/shock/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
gh attestation verify oci://ghcr.io/yuriyostapenko/charts/shock:X.Y.Z --repo yuriyostapenko/shock
```

`helm template` and `helm install` both succeed with the Sandbox CRD absent, so
GitOps engines can converge in any order.

## Images

**SHOCK image** (`orchestrator.image`, built from `images/orchestrator/Dockerfile`):
`gcr.io/distroless/base-debian13:nonroot` carrying `claude` for the orchestrator
process, the `shock` binary for the hook and the session controller, and
`/hooks/spawn-runner` as a symlink to `shock`, which runs the hook when invoked
under that name. The image has no shell or package manager and runs as user
65532. The orchestrator Deployment leaves `/hooks` to the image.

**Default runner image** (`runner.image`, built from `images/runner/Dockerfile`,
published as `ghcr.io/yuriyostapenko/shock-runner:X.Y.Z` and pinned by digest
in the released chart). It follows Anthropic's recipe: `debian:trixie-slim`,
`git` 2.47 and `openssh-client`, the native `claude` binary verified against
the release manifest, `libicu76` and `LANG=C.UTF-8` (ICU and a UTF-8 locale,
which .NET and other runtimes installed later expect), `procps` (`ps`, `pgrep`,
`pkill`, used by Claude Code and by scripts managing background processes),
and the doc's system git configuration. User `runner`
(uid 1000, the chart's default `fsGroup`) owns its home, `/home/runner`, and
two user-space package managers are on `PATH` so sessions can install tooling
without root:

- `mise` installs language runtimes and CLIs into `~/.local/share/mise`
  (`mise use -g node@22`, `mise use -g go@latest`, `mise use -g jq`).
- `uv` and `uvx` install Python versions and Python tools into `~/.local`.

The image entrypoint sets `CLAUDE_CODE_TMPDIR` to `~/.cache/claude-tmp`
(unless already set) before starting the CLI, so the scratchpad and task state
persist across sleep like the rest of the home.

The chart mounts the session's PVC at the runner user's home
(`runner.storage.mountPath`, default `/home/runner`) and checks repositories out
below it (`runner.baseDir`, default `/home/runner/workspace`). Everything a
session installs or configures under `~` therefore survives sleep and is already
there on resume: `mise` runtimes, `uv` tools and interpreters, the npm cache,
and any other dotfile. The runner's hard reset on resume touches only the
repository directory. Two consequences: the storage class must not mount volumes
`noexec`, since runtimes execute from the disk; and credentials that tools cache
in dotfiles (`~/.npmrc`, `~/.netrc`, `~/.docker/config.json`) stay on the disk
until the session is garbage-collected, so prefer short-lived credentials from a
wrapper. With the git proxy on, the runner wipes `~/.gitconfig` and
`~/.config/git` at startup.

`apt` is present but needs root. On clusters that support Pod user namespaces
(Kubernetes 1.36 GA; containerd 2.0+ or CRI-O 1.25+, kernel 6.3+ with
idmapped mounts), set `runner.hostUsers: false` together with
`runner.securityContext.runAsUser: 0` and `runAsNonRoot: false`: root inside
the container is then an unprivileged host user, `apt-get install` works, and
Pod Security Standards waive the non-root requirement for such Pods.

**Bring your own runner image** with `FROM ghcr.io/yuriyostapenko/shock-runner:X.Y.Z`
and add toolchains as root before switching back to `USER 1000`. Whatever the
image, the contract is: `claude` at 2.1.224 or later, pinned; `git >= 2.32`; a
non-root user whose `$HOME` is `runner.storage.mountPath`, the PVC, with
`runner.baseDir` inside it; and, optionally, a credentials wrapper wired with
`runner.extraArgs: ["--exec-path", "/opt/claude/wrapper.sh"]`.

## How a session runs

1. The orchestrator claims a spawn request and runs `/hooks/spawn-runner`. The
   hook strict-decodes `/etc/shock/sandbox-template.yaml`, stamps the session
   identity and forces the template contract, and creates the Sandbox
   **Suspended** with `pending-spawn`, `pending-spawn-at`, `last-order-id` and
   `last-order-attempt` in the create body. It then creates the immutable
   work-order Secret owned by the Sandbox UID and records `pending-secret`.
   It exits without waiting for anything.
2. The session controller sees a Suspended Sandbox with current-generation
   `Suspended=True` and a pending order, validates the Secret against order,
   attempt, session and Sandbox UID, and in one conditional patch sets
   `operatingMode: Running`, `applied-spawn` and the Pod template's order and
   Secret reference.
3. agent-sandbox creates the Pod and mounts `workspace-<sandbox>`. The controller
   observes an owned Pod carrying the order and clears pending intent. Readiness
   is never consulted.
4. The runner exits when the session is released or idle. agent-sandbox reports
   `Finished=True`; the controller patches `operatingMode: Suspended` and stamps
   `last-suspended-at`. The Pod is deleted; the PVC and Sandbox stay.
5. A later spawn request for the same session finds the Sandbox, publishes a new
   immutable Secret and pending order, requests suspension, and the controller
   wakes it once suspension is confirmed. The canonical clone is already on
   disk, so the runner does a fetch and hard reset rather than a fresh clone.
6. After `sessionController.gc.maxIdle` asleep the Sandbox is deleted with UID
   and resourceVersion preconditions. The PVC and every work-order Secret
   cascade by ownerReference. Recreating the session later starts with a new disk.

The Sandbox `operatingMode` alone is never trusted: every lifecycle decision
also requires the relevant condition to be True for the current
`metadata.generation`, so a stale condition or a pod still terminating cannot
produce two pods on one PVC.

### Bounce against a live session

A new spawn request against a still-running session patches `operatingMode:
Suspended`; the old Pod drains (`Suspended=False/PodTerminating`) and only after
`Suspended=True` does Wake install the new order. The pod count never exceeds
one. The hook never rotates a running Pod's JWT.

### Template changes reach sessions at their next spawn

When the hook accepts a higher attempt for an existing Sandbox it also installs
the currently rendered pod template (image, flags, resources), carrying over
the order currently on the Pod. A `helm upgrade` therefore never touches
sleeping Sandboxes; they pick the new template up on their next spawn.
`volumeClaimTemplates` is immutable: storage changes apply to new sessions only.

## Values

See `values.yaml` for the full, commented list; `values.schema.json` enforces
types and enums. The load-bearing ones:

| Key | Default | Notes |
| --- | --- | --- |
| `environment.existingSecret` | `""` | Secret with key `environment-secret`. Required unless `secretValue` is set. |
| `orchestrator.image.tag`, `runner.image.tag` | `""` | Fall back to the chart's `appVersion`. The `digest` fields pin the images; the release sets them. |
| `runner.hostUsers` | unset | `false` runs the runner Pod in a user namespace so root inside the container can use `apt`. |
| `runner.flags.useAnthropicGitProxy` | `true` | Git goes through `api.anthropic.com` with the session creator's GitHub connection; the runner holds no git credentials. Set `false` when supplying credentials yourself. |
| `orchestrator.expectedSpawnSeconds` | `180` | Server-side spawn lease, shared by all replicas. Must exceed `hookTimeout + 5` (rendering fails otherwise). Includes the initial suspension round-trip. |
| `orchestrator.hookTimeout` | `30` | The hook keeps its API work within 80% of this. |
| `sessionController.gc.maxIdle` | `336h` | Sandbox, PVC and Secrets are deleted after 14 days asleep. |
| `sessionController.zombie.alertAfter` | `5m` | Pods Terminating longer than this raise an Event and alert. SHOCK never force-deletes. |
| `runner.storage.mountPath` | `/home/runner` | Where the per-session PVC is mounted: the runner user's home. |
| `runner.baseDir` | `/home/runner/workspace` | The runner's `--base-dir`, at or below the mount path. Same on every runner. |
| `runner.terminationGracePeriodSeconds` | `120` | Runner SIGKILL floor is 75 s at defaults, 105 s with push-outcome. |
| `runner.storage.accessMode` | `ReadWriteOncePod` | Immutable per session. Decide before first install. |
| `runner.instructions` | environment notes | Markdown mounted at `/etc/claude-code/CLAUDE.md` in every runner Pod, Claude Code's managed-policy instructions: what Claude should know about this runner (persistent home, root-free installs, git proxy). Empty mounts nothing. |
| `runner.podTemplate` | `{}` | Deep-merged over the rendered pod template (maps merge, lists replace). |
| `network.allowedFQDNs` | hosts the Trusted list lacks: Anthropic downloads and docs, GitHub asset and ghcr layer hosts, mise metadata, Go and .NET downloads, registry layer hosts | `host` or `host:port`; a leading `*.` matches every subdomain. This deployment's own list, disjoint from Anthropic's; see values.yaml. |
| `network.anthropicTrustedDomains` | `true` | Also allow Anthropic's Trusted-level default domains from `files/anthropic-trusted-domains.txt`. |
| `network.enforceSNI` | `true` | cilium mode: TLS to an `allowedFQDNs` entry must carry a matching SNI; wildcard entries are enforced as patterns. |
| `network.mode` | `cilium` | `kubernetes` renders plain NetworkPolicy without FQDN rules; `none` renders nothing. |

### `runner.podTemplate` and the template contract

Helm merges `runner.podTemplate` over the rendered template; the hook then
forces these fields at spawn time, so an overlay cannot break the lifecycle:

| Field | Forced to |
| --- | --- |
| `spec.restartPolicy` | `Never` |
| `spec.automountServiceAccountToken` | `false` |
| `spec.terminationGracePeriodSeconds` | `runner.terminationGracePeriodSeconds` |
| `metadata.labels` | the common and session label set, merged last |
| `runner` container `workspace` mount path | `runner.storage.mountPath`; `runner.baseDir` must be at or below it |
| `runner` container `work-order` mount | `/var/run/claude/work-order`, read-only |
| `spec.operatingMode` on creation | `Suspended` |

Two anchors are required and must keep their names: the container `runner`
and the volume `work-order`. Removing or renaming either makes the hook exit 2
with a message naming the anchor, before anything is written. Users keep
everything else: resources, nodeSelector, tolerations, affinity,
priorityClassName, imagePullSecrets, serviceAccountName, securityContext, extra
labels, annotations, volumes and mounts. Because lists replace on merge, prefer
the dedicated values (`runner.resources`, `runner.extraEnv`, `runner.extraVolumes`,
`runner.extraVolumeMounts`, `runner.extraArgs`) over overriding `containers`.

## Namespace, names and RBAC

- Dedicate the namespace to SHOCK. The hook's Role grants `secrets create` and
  `sandboxes create/patch` namespace-wide because RBAC cannot prefix-match names.
- Sandbox names are `<release>-cs-<sanitized session id>` (RFC 1123, at most 63
  chars, plus an 8-char hash when sanitizing or truncating changed the id), so
  releases never collide on names. A session still belongs to exactly one
  release: two releases serving the same Anthropic environment in one namespace
  is a misconfiguration and the hook refuses a Sandbox labeled for another
  release with exit 2. Two releases serving different environments can share a
  namespace; every selector includes `app.kubernetes.io/instance`.
- Work-order Secrets are `wo-<sha256(release, session, Sandbox UID, order)>`,
  immutable, owned by the Sandbox (non-controller, `blockOwnerDeletion: false`).
  One Secret per attempted order lives until the Sandbox is collected; size
  Secret quotas accordingly. A recreated Sandbox derives different names and
  cannot adopt old credentials.
- The session controller can `patch` and (with GC enabled) `delete` Sandboxes,
  `get` Secrets and `get/list/watch` Pods. It never deletes Pods or Secrets.
- Runner Pods never receive a service-account token.

## Registry credentials

1. **Pull-through mirror (recommended).** Bake mirror URLs into the runner
   image or inject them with `runner.extraEnv`. Add the mirror host to
   `network.allowedFQDNs`; otherwise installs hang against default-deny egress
   and fail on timeout instead of a clear error.
2. **Wrapper.** `runner.extraArgs: ["--exec-path", "/opt/claude/wrapper.sh"]`;
   mount the credentials Secret with `runner.extraVolumes` and
   `runner.extraVolumeMounts`. The wrapper materializes `~/.npmrc`,
   `NuGet.config` or a docker config before `exec "$CLAUDE_RUNNER_CLAUDE_BIN" "$@"`.
   Never bake broad push tokens into the shared image.

## Network policy

`cilium` mode renders one `CiliumNetworkPolicy` per component, selected by the
release-scoped selector labels: default-deny egress, DNS to kube-dns, `toFQDNs`
for every `network.allowedFQDNs` entry on its port (443 default), an explicit
L3 deny for `169.254.169.254/32`, and `kube-apiserver` for the orchestrator and
session controller. This governs egress from the Pod; image pulls are the
kubelet's traffic and do not belong here.

Two lists feed the runner's `toFQDNs` rules. `network.allowedFQDNs` is this
deployment's own list. `network.anthropicTrustedDomains` (default `true`) adds
Anthropic's Trusted-level default domains, the allow list of Anthropic-hosted
environments, kept verbatim in `files/anthropic-trusted-domains.txt` with its
source and fetch date so it can be refreshed with `make trusted-domains`
without touching your list. It is broad (`*.amazonaws.com`, `*.googleapis.com`
and other object stores are on it); set it to `false` for a default-deny
posture that allows only what you list. The default `allowedFQDNs` holds only
what Anthropic's list lacks, so with the Trusted list off you list every host
yourself, and the chart refuses to render unless `api.anthropic.com` is among
them. Duplicates between the lists collapse into one rule.

`toFQDNs` admits the addresses a name resolved to, and CDN addresses are shared
between customers: a host that lands on the same address as an allowed one is
reachable by address. With `network.enforceSNI` (default) every entry also
carries `serverNames` with the same name or `*` pattern, so the TLS handshake
must name a matching host.

`kubernetes` mode renders plain `NetworkPolicy`: DNS plus any address on the
allowed ports except the metadata CIDR. There is no FQDN filtering; enforce host
allowlists at your network boundary.

## Monitoring

A `PodMonitor` scrapes the named `health` port at `/metrics` on orchestrator,
session controller and runner pods of this release. It drops the `email` label
from `claude_code_self_hosted_runner_locked_account` (PII) by default.

The session controller exports sandbox state from its informer cache, labeled by
Sandbox name only (no session or account ids):

| Series | Meaning |
| --- | --- |
| `shock_sandbox_condition_status{sandbox,type,reason,operating_mode,current}` | 1 True, 0 False, -1 Unknown; `current` is whether observedGeneration matches generation |
| `shock_sandbox_pending_spawn_age_seconds{sandbox}` | age of an unobserved order |
| `shock_sandbox_pod_terminating_seconds{sandbox,pod}` | stranded-pod detection |
| `shock_sandbox_info{sandbox,operating_mode,pending}`, `shock_sandboxes{operating_mode}` | inventory |
| `shock_crd_served` | 1 while the Sandbox CRD is served |
| `shock_build_info{version,revision,go_version}` | always 1; the git tag or pseudo-version and commit the binary was built from |

The `PrometheusRule` carries Anthropic's sample alerts (runner poll stale,
orchestrator disconnected, poll stale above `hookTimeout + 30`, circuit-broken,
spawn-hook failures) and SHOCK's: sleep transition failed (`Finished=True` while
Running for 2 min), MultiplePods, pending spawn older than
`expectedSpawnSeconds`, session controller not ready or erroring, and session
stranded (Pod Terminating beyond `zombie.alertAfter`). The last one needs a
cluster admin: SHOCK never taints nodes or force-deletes Pods.

Wake-latency SLO query:

```promql
histogram_quantile(0.99, sum by (le) (rate(claude_code_self_hosted_orchestrator_session_queue_wait_seconds_bucket[15m])))
  + histogram_quantile(0.99, sum by (le) (rate(claude_code_self_hosted_runner_session_init_duration_seconds_bucket[15m])))
```

Probes on `/healthz` detect a dead process only; the orchestrator's poll health
is `claude_code_self_hosted_orchestrator_connected`.

## Operations

- **Stranded session** (`ShockSessionStranded`): a Pod is stuck Terminating,
  usually on an unhealthy node. Drain or recover the node. Do not force-delete
  the Pod: it defeats the `Suspended=True` gate and the deterministic Pod name
  and is the only way to get two Pods on one PVC.
- **Circuit-broken sessions**: the hook exited 2 (template, RBAC, identity or
  immutable-Secret mismatch). The stderr tail shows in the environment's
  Activity tab. Fix the cause, then Retry there.
- **Upgrading agent-sandbox**: only versions in the e2e matrix are tested.
  Re-derive `kubeVersion`, re-run the conformance suite, and confirm no
  controller-driven idle-suspend applies by default before adding a version.
